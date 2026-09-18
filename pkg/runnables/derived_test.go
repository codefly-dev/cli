package runnables_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/runnables"
	"github.com/codefly-dev/cli/pkg/runnables/runnablestest"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestLoadDerivedOperationsProjectsTheServiceFacility(t *testing.T) {
	moduleDir := t.TempDir()
	pkg := runnablestest.Write(t, moduleDir, "test-workspace", "documents", "runtime-worker-grpc-apply-text")

	derived, err := runnables.LoadDerivedOperations(moduleDir)
	require.NoError(t, err)
	require.Len(t, derived, 1)

	identity := derived[0].Identity()
	require.Equal(t, "documents", identity.Module)
	require.Equal(t, "runtime-worker-grpc-apply-text", identity.Name)
	require.Equal(t, "0.1.0", identity.Version)
	require.Equal(t, "codefly.dev/go:0.0.48", identity.Agent)
	require.Equal(t, resources.RunnableServiceProtocolV1, identity.Protocol)
	// The facility is spelled as a declared runnable spells it, so one listing
	// does not name the same facility two ways.
	require.Equal(t, []string{"service"}, identity.Execution.Facilities)
	require.Equal(t, "1m0s", identity.Execution.Timeout)
	require.Equal(t, "receipt", identity.Execution.Recovery)
	require.Equal(t, "none", identity.Execution.Cancellation)
	require.Equal(t, "runtime-worker/grpc/ApplyText", identity.Source)
	require.Equal(t, pkg.GetDigest(), derived[0].Entry.Digest)
	require.Equal(t, "documents.ingestion", derived[0].Operation.Audience)
}

// A module that derived nothing has no index, which is an answer rather than a
// failure: the caller still lists what that module declares.
func TestLoadDerivedOperationsIsEmptyWithoutAnIndex(t *testing.T) {
	derived, err := runnables.LoadDerivedOperations(t.TempDir())
	require.NoError(t, err)
	require.Empty(t, derived)
}

func TestLoadIndexRefusesAnUnknownSchema(t *testing.T) {
	moduleDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(moduleDir, "contracts", "runnables"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "contracts", "runnables", runnables.IndexFileName),
		[]byte(`{"schema":"codefly/runnable-operations/v99"}`), 0o644))

	_, err := runnables.LoadIndex(moduleDir)
	require.ErrorContains(t, err, "codefly/runnable-operations/v99")
}

// CanonicalJSON is what makes --check a gate rather than a coin flip:
// protojson varies its whitespace between calls, so the file has to be the
// normalized form core digests, byte for byte, on every run.
func TestCanonicalJSONIsStableAndSorted(t *testing.T) {
	pkg := runnablestest.Package(t, "test-workspace", "documents", "runtime-worker-grpc-apply-text")
	first, err := runnables.CanonicalJSON(pkg)
	require.NoError(t, err)
	for range 20 {
		again, againErr := runnables.CanonicalJSON(pkg)
		require.NoError(t, againErr)
		require.Equal(t, string(first), string(again))
	}
	require.True(t, json.Valid(first))
	// Proto field names, sorted: "agent" comes before "contract" comes before
	// "digest", whatever order the message declares them in.
	require.Less(t, indexOf(t, first, `"agent"`), indexOf(t, first, `"contract"`))
	require.Less(t, indexOf(t, first, `"contract"`), indexOf(t, first, `"digest"`))
	require.Contains(t, string(first), `"service_operations"`)
}

func indexOf(t *testing.T, data []byte, needle string) int {
	t.Helper()
	at := -1
	for i := 0; i+len(needle) <= len(data); i++ {
		if string(data[i:i+len(needle)]) == needle {
			at = i
			break
		}
	}
	require.GreaterOrEqual(t, at, 0, "%s not found", needle)
	return at
}
