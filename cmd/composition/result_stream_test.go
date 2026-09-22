package composition

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
)

// Every verb here encodes its result as JSON to stdout, and the process-group
// reaper narrates on the same stream while a command runs. A single narrated
// line lands inside the encoder's output and makes the result unparseable for
// `| jq` and for the tests that read it back.
func TestCompositionResultStreamSurvivesConcurrentNarration(t *testing.T) {
	read, write, err := os.Pipe()
	require.NoError(t, err)
	stdout := os.Stdout
	os.Stdout = write
	t.Cleanup(func() { os.Stdout = stdout })

	restore := cli.ProtectResultStream()
	cli.Info("reaping stale managed process group record=%s", "/runs/2049.pgid")
	cli.Focus("reconciled managed process group")
	restore()

	require.NoError(t, write.Close())
	narrated, err := io.ReadAll(read)
	require.NoError(t, err)
	require.Empty(t, string(narrated), "narration reached the stream carrying the command result")
}

// The guard has to hand the stream back, or a caller that runs a command and
// then prints for a human goes silent.
func TestCompositionResultStreamIsRestoredAndNests(t *testing.T) {
	outer := cli.ProtectResultStream()
	inner := cli.ProtectResultStream()
	inner()

	var narrated strings.Builder
	cli.SetOutputSink(func(_ wool.Loglevel, message string) { narrated.WriteString(message) })
	cli.Info("still guarded")
	require.NotEmpty(t, narrated.String(), "the inner undo handed stdout back while the outer guard still held it")

	cli.SetOutputSink(nil)
	outer()
	require.False(t, cli.OutputSuppressed(), "the outer undo left narration suppressed")
}

// A result the command produced must round-trip: the guard must not swallow
// the encoder's own output along with the narration.
func TestCompositionResultStreamKeepsTheEncodedResult(t *testing.T) {
	read, write, err := os.Pipe()
	require.NoError(t, err)
	stdout := os.Stdout
	os.Stdout = write
	t.Cleanup(func() { os.Stdout = stdout })

	restore := cli.ProtectResultStream()
	cli.Info("narration that must not reach stdout")
	require.NoError(t, json.NewEncoder(os.Stdout).Encode(map[string]string{"identity": "sha256:abc"}))
	restore()

	require.NoError(t, write.Close())
	emitted, err := io.ReadAll(read)
	require.NoError(t, err)
	var decoded map[string]string
	require.NoError(t, json.Unmarshal(emitted, &decoded))
	require.Equal(t, "sha256:abc", decoded["identity"])
}
