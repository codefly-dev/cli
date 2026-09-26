// Package protocoltest provides test-only peers for CLI wire-boundary tests.
// It never installs or downloads a released plugin.
package protocoltest

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type Call struct {
	Method  string          `json:"method"`
	Request json.RawMessage `json:"request"`
	PID     int             `json:"pid"`
}

// Install builds the CLI-owned protocol peer into a disposable host profile.
func Install(t *testing.T, names ...string) []string {
	t.Helper()
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	t.Setenv("CODEFLY_TEST_PEER_ROOT", t.TempDir())
	_, source, _, ok := runtime.Caller(0)
	require.True(t, ok)
	binary := filepath.Join(t.TempDir(), "protocol-peer")
	command := exec.CommandContext(t.Context(), "go", peerBuildArgs(binary)...)
	command.Dir = filepath.Dir(source)
	command.Env = append(os.Environ(), "GOWORK=off")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	identifiers := make([]string, 0, len(names))
	for _, name := range names {
		selection := &resources.Agent{Kind: resources.ServiceAgent, Publisher: "example.test", Name: name, Version: "0.0.1"}
		path, err := selection.Path(t.Context())
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.Symlink(binary, path))
		identifiers = append(identifiers, selection.Identifier())
	}
	return identifiers
}

// peerBuildArgs builds the peer with the race detector exactly when the test
// binary has it. The race job still races the peer; every other job builds it
// from the same cache entries its own test binary already filled. A
// hard-coded -race made a job without a race-instrumented cache (a cold
// coverage run) compile the peer's whole dependency graph under -race in every
// package that installs it, which alone took about three minutes on a CI
// runner and pushed pkg/gateway past its four-minute test timeout.
func peerBuildArgs(binary string) []string {
	args := []string{"build"}
	if raceEnabled {
		args = append(args, "-race")
	}
	return append(args, "-o", binary, "./testdata/peer")
}

func Response(t *testing.T, root, name string, response proto.Message) {
	t.Helper()
	data, err := protojson.Marshal(response)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".protocol-test"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".protocol-test", name+".json"), data, 0o600))
}

func Calls(t *testing.T, root string) []Call {
	t.Helper()
	file, err := os.Open(filepath.Join(root, ".protocol-test", "calls.jsonl"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	decoder := json.NewDecoder(file)
	var calls []Call
	for {
		var call Call
		err := decoder.Decode(&call)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		calls = append(calls, call)
	}
	return calls
}
