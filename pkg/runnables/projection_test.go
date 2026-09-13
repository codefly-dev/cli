package runnables_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

const declaration = `kind: runnable
name: word-count
version: 0.1.0
description: Count words
agent:
  kind: codefly:runnable
  name: python
  version: 0.0.1
  publisher: codefly.dev
contract:
  protocol: codefly.runnable/v1
  input:
    fields:
      - name: text
        type: string
  output:
    fields:
      - name: count
        type: integer
entrypoint:
  handler: handler.py
execution:
  facilities: [native, kubernetes]
  timeout: 2m
  cancellation: signal
  recovery: recompute
  concurrency: 4
  payload:
    max-input-bytes: 65536
`

func loadRunnable(t *testing.T) *resources.Runnable {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "runnable.codefly.yaml"), []byte(declaration), 0o644))
	runnable, err := resources.LoadRunnableFromDir(context.Background(), dir)
	require.NoError(t, err)
	runnable.SetModule("backend")
	return runnable
}

func TestIdentityCarriesEveryFieldASurfaceReports(t *testing.T) {
	identity := runnablespkg.NewIdentity(loadRunnable(t))

	require.Equal(t, "backend", identity.Module)
	require.Equal(t, "word-count", identity.Name)
	require.Equal(t, "0.1.0", identity.Version)
	require.Equal(t, "Count words", identity.Description)
	require.Equal(t, "codefly.dev/python:0.0.1", identity.Agent)
	require.Equal(t, "codefly.runnable/v1", identity.Protocol)
	require.Equal(t, []string{"native", "kubernetes"}, identity.Execution.Facilities)
	require.Equal(t, "2m", identity.Execution.Timeout)
	require.Equal(t, "signal", identity.Execution.Cancellation)
	require.Equal(t, "recompute", identity.Execution.Recovery)
	require.Equal(t, uint32(4), identity.Execution.Concurrency)
}

// TestPayloadBoundsAreEffectiveNotDeclared: core substitutes a default for an
// undeclared payload limit, so reporting the blank would tell a caller nothing
// about the bound its payload must actually fit in.
func TestPayloadBoundsAreEffectiveNotDeclared(t *testing.T) {
	identity := runnablespkg.NewIdentity(loadRunnable(t))
	require.Equal(t, uint64(65536), identity.Execution.MaxInputBytes, "declared bound")
	require.Equal(t, resources.DefaultRunnablePayloadBytes, identity.Execution.MaxOutputBytes, "defaulted bound")
}

// TestFacilitiesMarshalAsAnArrayWhenEmpty keeps the JSON shape stable for a
// machine caller: a nil slice marshals to null, which is not a list.
func TestFacilitiesMarshalAsAnArrayWhenEmpty(t *testing.T) {
	data, err := json.Marshal(runnablespkg.NewExecution(&resources.RunnableExecution{Timeout: "1m"}))
	require.NoError(t, err)
	require.Contains(t, string(data), `"facilities":[]`)
}

// TestIdentityJSONKeysArePinned is the drift guard. Every runnable listing
// surface — `codefly list runnables --json`, `codefly show runnable --json`
// and the MCP list_runnables tool — marshals this one type, so these keys are
// the contract all three publish. Renaming a field here changes all three at
// once, which is the point; doing it silently is not.
func TestIdentityJSONKeysArePinned(t *testing.T) {
	data, err := json.Marshal(runnablespkg.NewIdentity(loadRunnable(t)))
	require.NoError(t, err)
	var object map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &object))

	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	require.ElementsMatch(t,
		[]string{"module", "name", "version", "description", "agent", "protocol", "execution"},
		keys)

	var execution map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(object["execution"], &execution))
	executionKeys := make([]string, 0, len(execution))
	for key := range execution {
		executionKeys = append(executionKeys, key)
	}
	require.ElementsMatch(t,
		[]string{"facilities", "timeout", "cancellation", "recovery", "concurrency", "max_input_bytes", "max_output_bytes"},
		executionKeys)
}
