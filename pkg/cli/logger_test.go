package cli

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

func TestMilestoneRendersPortAndElapsedInline(t *testing.T) {
	cases := []struct {
		name    string
		port    int
		elapsed time.Duration
		want    string
	}{
		{"plain", 0, 0, ">> infra/postgres: Running"},
		{"port", 41840, 0, ">> infra/postgres: Running on :41840"},
		{"elapsed", 0, 1234 * time.Millisecond, ">> infra/postgres: Running in 1.2s"},
		{"port and elapsed", 41840, 1234 * time.Millisecond, ">> infra/postgres: Running on :41840 in 1.2s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Milestone("infra/postgres", "Running", tc.port, tc.elapsed)
			if got != tc.want {
				t.Fatalf("Milestone = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMilestoneEmitterSuppressesConsecutiveRepeats(t *testing.T) {
	e := NewMilestoneEmitter()

	if !e.Emit("infra/postgres", ">> infra/postgres: Running on :41840") {
		t.Fatal("first emit should print")
	}
	if e.Emit("infra/postgres", ">> infra/postgres: Running on :41840") {
		t.Fatal("identical consecutive emit should be suppressed")
	}
	// A different line for the same service prints again.
	if !e.Emit("infra/postgres", ">> infra/postgres: Starting") {
		t.Fatal("changed milestone should print")
	}
	// The same line as before is no longer the *previous* one, so it prints.
	if !e.Emit("infra/postgres", ">> infra/postgres: Running on :41840") {
		t.Fatal("non-consecutive repeat should print")
	}
	// Another service's history is independent.
	if !e.Emit("mind/mind", ">> mind/mind: Running") {
		t.Fatal("first emit for a second service should print")
	}
}

func TestLogUsesNarrationMarkerForForward(t *testing.T) {
	oldMax := maxUnique
	maxUnique = len("infra/postgres")
	defer func() { maxUnique = oldMax }()
	defer withTimestamps(false)()

	lines := formattedLogLines("infra/postgres", MarkerNarration, &wool.Log{
		Level:   wool.FORWARD,
		Message: "database ready",
	})

	if len(lines) != 1 || lines[0] != "infra/postgres > database ready" {
		t.Fatalf("unexpected rendered line: %#v", lines)
	}
}

func TestLegendNamesEveryMarker(t *testing.T) {
	legend := Legend()
	for _, marker := range []string{MarkerMilestone, MarkerNarration, MarkerService} {
		if !strings.Contains(legend, marker) {
			t.Fatalf("legend %q missing marker %q", legend, marker)
		}
	}
}

func TestSuppressedLoggerRoutesUnsourcedLogsToOutputSink(t *testing.T) {
	logger := &Logger{suppressed: true}
	var gotLevel wool.Loglevel
	var gotMessage string
	SetOutputSink(func(level wool.Loglevel, msg string) {
		gotLevel = level
		gotMessage = msg
	})
	defer SetOutputSink(nil)

	logger.Process(&wool.Log{
		Level:   wool.INFO,
		Message: "loading service agent codefly.dev/go-grpc:0.1.4",
	})

	if gotLevel != wool.INFO {
		t.Fatalf("expected INFO level, got %s", gotLevel)
	}
	if !strings.Contains(gotMessage, "codefly.dev/go-grpc:0.1.4") {
		t.Fatalf("sink did not receive rendered log message, got %q", gotMessage)
	}
}

// NOTE: these tests mutate the package-level maxUnique global to
// pin the padding width. Do NOT call t.Parallel() in this file —
// concurrent assignment to maxUnique races and produces flaky
// width assertions. If you need parallelism here, refactor
// formattedLogLines to take width as a parameter instead.

func TestFormattedLogLinesTrimsForwardNewlines(t *testing.T) {
	oldMax := maxUnique
	maxUnique = len("infra/postgres")
	defer func() { maxUnique = oldMax }()
	defer withTimestamps(false)()

	lines := formattedLogLines("infra/postgres", ">", &wool.Log{
		Level:   wool.FORWARD,
		Message: "database ready\n\n",
	})

	if len(lines) != 1 {
		t.Fatalf("expected one rendered line, got %d: %#v", len(lines), lines)
	}
	if lines[0] != "infra/postgres > database ready" {
		t.Fatalf("unexpected rendered line: %q", lines[0])
	}
}

func TestFormattedLogLinesPrefixesEachMultilineEntry(t *testing.T) {
	oldMax := maxUnique
	maxUnique = len("infra/postgres")
	defer func() { maxUnique = oldMax }()
	defer withTimestamps(false)()

	lines := formattedLogLines("infra/postgres", ">", &wool.Log{
		Level:   wool.FORWARD,
		Message: "first\nsecond\n",
	})

	expected := []string{
		"infra/postgres > first",
		"infra/postgres > second",
	}
	if len(lines) != len(expected) {
		t.Fatalf("expected %d lines, got %d: %#v", len(expected), len(lines), lines)
	}
	for i := range expected {
		if lines[i] != expected[i] {
			t.Fatalf("line %d: expected %q, got %q", i, expected[i], lines[i])
		}
	}
}

// withTimestamps sets the package-level timestamp toggle and returns a
// restore func, so a test can pin it without leaking into others.
func withTimestamps(on bool) func() {
	prev := timestamps.Load()
	timestamps.Store(on)
	return func() { timestamps.Store(prev) }
}

func TestFormattedLogLinesPrependsWallClockWhenEnabled(t *testing.T) {
	oldMax := maxUnique
	maxUnique = len("infra/postgres")
	defer func() { maxUnique = oldMax }()
	defer withTimestamps(true)()

	lines := formattedLogLines("infra/postgres", ">", &wool.Log{
		Level:   wool.FORWARD,
		Message: "database ready",
	})

	if len(lines) != 1 {
		t.Fatalf("expected one rendered line, got %d: %#v", len(lines), lines)
	}
	want := regexp.MustCompile(`^\d{2}:\d{2}:\d{2} infra/postgres > database ready$`)
	if !want.MatchString(lines[0]) {
		t.Fatalf("line missing wall-clock prefix: %q", lines[0])
	}
}

// TestWithSilence_ConcurrentLog_RaceFree regression-covers the
// silentMu lock added to the package-level silent slice. Before the
// fix, WithSilence appended without synchronization while Log read
// the slice — race detector fired immediately under concurrent
// access. The test reproduces that exact pattern: callers logging
// while WithSilence is invoked from another goroutine.
func TestWithSilence_ConcurrentLog_RaceFree(t *testing.T) {
	stop := make(chan struct{})
	done := make(chan struct{})

	// Caller: log forward-level entries that exercise the silent-slice read.
	go func() {
		defer close(done)
		ident := &wool.Identifier{Unique: "infra/postgres"}
		entry := &wool.Log{Level: wool.FORWARD, Message: "tick"}
		for {
			select {
			case <-stop:
				return
			default:
				Log(ident, entry)
			}
		}
	}()

	// Mutator: append to the silent slice. Race detector watches the
	// concurrent write/read on the package-level slice.
	svcs := []*resources.ServiceWithModule{
		{Name: "redis", Module: "infra"},
		{Name: "postgres", Module: "infra"},
	}
	for i := 0; i < 200; i++ {
		WithSilence(svcs)
	}

	close(stop)
	<-done
}
