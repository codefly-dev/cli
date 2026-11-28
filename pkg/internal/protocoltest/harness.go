// Package protocoltest provides test-only peers for CLI wire-boundary tests.
// It never installs or downloads a released plugin.
package protocoltest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/codefly-dev/core/agents/manager"
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

// Keep the executable bytes for the lifetime of the test binary. Each
// installation still owns its executable and the mutable state of its peers.
var peerExecutable = sync.OnceValues(func() ([]byte, error) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return nil, errors.New("resolve protocol peer build directory")
	}
	dir, err := os.MkdirTemp("", "codefly-protocol-build-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	binary := filepath.Join(dir, "protocol-peer")
	command := exec.Command("go", peerBuildArgs(binary)...) //nolint:gosec // G204: internal build arguments and a temporary output path
	command.Dir = filepath.Dir(source)
	command.Env = append(os.Environ(), "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("build protocol peer: %w: %s", err, output)
	}
	return os.ReadFile(binary)
})

// Install stages the CLI-owned protocol peer into a disposable host profile.
// The peer is a local build under test names no release stands for, so the
// test runs it as one deliberately (CODEFLY_AGENT_SOURCE=local): under remote
// resolution core fetches a cached agent's release over it.
func Install(t *testing.T, names ...string) []string {
	t.Helper()
	executable, err := peerExecutable()
	require.NoError(t, err)
	t.Setenv(manager.AgentSourceEnv, "local")
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	t.Setenv("CODEFLY_TEST_PEER_ROOT", t.TempDir())
	binary := filepath.Join(t.TempDir(), "protocol-peer")
	require.NoError(t, os.WriteFile(binary, executable, 0o700)) //nolint:gosec // G306: the test subprocess needs owner-only execute permission
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
// runner and pushed the largest package past its four-minute test timeout.
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
