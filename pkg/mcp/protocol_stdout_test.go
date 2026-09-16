package mcp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/wool"
)

// captureStdio redirects both standard streams for the duration of fn and
// returns what each collected. Both are needed: the point of the guard is not
// that a line disappears but that it moves off stdout and onto stderr.
func captureStdio(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	originalOut, originalErr := os.Stdout, os.Stderr
	outReader, outWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errReader, errWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outWriter, errWriter

	outCollected := make(chan string, 1)
	errCollected := make(chan string, 1)
	go func() {
		var buffer strings.Builder
		_, _ = io.Copy(&buffer, outReader)
		outCollected <- buffer.String()
	}()
	go func() {
		var buffer strings.Builder
		_, _ = io.Copy(&buffer, errReader)
		errCollected <- buffer.String()
	}()

	fn()

	os.Stdout, os.Stderr = originalOut, originalErr
	_ = outWriter.Close()
	_ = errWriter.Close()
	stdout, stderr = <-outCollected, <-errCollected
	_ = outReader.Close()
	_ = errReader.Close()
	return stdout, stderr
}

// The guard has to be installed before the server is CONSTRUCTED, not before it
// serves. NewServer logs from contexts carrying no provider — its own "no
// workspace loaded" line, and, inside engine.NewWorkspaceHost, the stale-process
// reaper's, which runs with a hardcoded context.Background(). All of that is
// already on stdout by the time Serve runs, so a guard installed there misses
// the very lines it exists for.
func TestProtectStdoutCoversConstructionNotJustServing(t *testing.T) {
	// A directory with no workspace, so construction has something to say.
	t.Chdir(t.TempDir())
	previousLevel := wool.GlobalLogLevel()
	wool.SetGlobalLogLevel(wool.DEBUG)
	t.Cleanup(func() { wool.SetGlobalLogLevel(previousLevel) })

	construct := func() {
		server, err := NewServer(context.Background(), "test")
		if err != nil {
			t.Errorf("NewServer: %v", err)
			return
		}
		_ = server.Close()
	}

	// Unguarded, construction writes to the stream Serve would be serving
	// JSON-RPC on. This is the defect, reproduced.
	unguarded, _ := captureStdio(t, construct)
	if unguarded == "" {
		t.Fatal("construction reached stdout with nothing; the test no longer reproduces the corruption it guards")
	}

	guarded, guardedErr := captureStdio(t, func() {
		defer ProtectStdout()()
		construct()
	})
	if guarded != "" {
		t.Errorf("construction wrote %q to stdout, corrupting the JSON-RPC stream", guarded)
	}
	if guardedErr == "" {
		t.Error("construction logs were dropped rather than routed; the operator needs them on stderr")
	}
}

// Every spawned agent's logs are fanned to the processors registered with
// agents.AddProcessor, and pkg/cli registers a stdout-printing one in its init.
// That path is neither a wool fallback nor a context the plane hands out, so
// neither of the other guards reaches it — and once a run starts it is by far
// the loudest writer to the protocol stream.
func TestProtectStdoutKeepsForwardedAgentLogsOffStdout(t *testing.T) {
	line, err := json.Marshal(agents.LogMessage{
		Source: &wool.Identifier{Kind: "service", Unique: "wiki/backend"},
		Log:    &wool.Log{Level: wool.INFO, Message: "agent log on the protocol stream"},
	})
	if err != nil {
		t.Fatal(err)
	}
	forward := func() {
		reader, writer := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			agents.GetLogHandler().ForwardLogs(reader)
		}()
		if _, err := writer.Write(append(line, '\n')); err != nil {
			t.Error(err)
		}
		_ = writer.Close()
		<-done
	}

	unguarded, _ := captureStdio(t, forward)
	if !strings.Contains(unguarded, "agent log on the protocol stream") {
		t.Fatalf("a forwarded agent log reached stdout with %q; the test no longer reproduces the corruption it guards", unguarded)
	}

	guarded, guardedErr := captureStdio(t, func() {
		defer ProtectStdout()()
		forward()
	})
	if guarded != "" {
		t.Errorf("a forwarded agent log wrote %q to stdout, corrupting the JSON-RPC stream", guarded)
	}
	if !strings.Contains(guardedErr, "agent log on the protocol stream") {
		t.Errorf("a forwarded agent log was dropped instead of routed to stderr; stderr had %q", guardedErr)
	}
}

// The undo has to actually undo: a process that runs the server in-process and
// then goes on to do something else must not inherit a stderr-pinned fallback
// and a permanently muted CLI logger.
func TestProtectStdoutUndoRestoresStdoutLogging(t *testing.T) {
	restore := ProtectStdout()
	restore()

	after, _ := captureStdio(t, func() {
		wool.Get(context.Background()).In("orphan").Error("back on stdout")
	})
	if !strings.Contains(after, "back on stdout") {
		t.Errorf("the guard stayed installed after its undo ran; stdout had %q", after)
	}
}

// wool.process already filtered this line against the effective level, which is
// what makes CODEFLY_LOG's per-scope overrides work. Checking the global level
// again here drops exactly the lines a scope override was set to surface.
func TestProtocolSafeLoggerDoesNotRefilterAScopedLine(t *testing.T) {
	previousLevel := wool.GlobalLogLevel()
	wool.SetGlobalLogLevel(wool.INFO)
	t.Cleanup(func() { wool.SetGlobalLogLevel(previousLevel) })

	_, stderr := captureStdio(t, func() {
		protocolSafeLogger{}.Process(&wool.Log{Level: wool.DEBUG, Message: "scope-enabled debug line"})
	})
	if !strings.Contains(stderr, "scope-enabled debug line") {
		t.Errorf("a line wool deliberately let through was filtered again here; stderr had %q", stderr)
	}
}
