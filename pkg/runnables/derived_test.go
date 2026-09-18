package runnables_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// writeIndexWithPath writes an index whose single row points wherever the
// caller says, leaving the package and policy files where the generator would
// have put them.
func writeIndexWithPath(t *testing.T, moduleDir, rowPath string) {
	t.Helper()
	index := `{"schema":"` + runnables.IndexSchema + `","workspace":"w","module":"m","operations":[` +
		`{"name":"x","version":"0.1.0","digest":"d","service":"s","endpoint":"grpc","method":"/p.S/M",` +
		`"input_message":"a","output_message":"b","path":"` + rowPath + `"}]}`
	require.NoError(t, os.MkdirAll(filepath.Join(moduleDir, "contracts", "runnables"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "contracts", "runnables", runnables.IndexFileName), []byte(index), 0o600))
}

// An index is data on disk — in a composed workspace it arrives inside a
// third-party module package — and a decode failure quotes the bytes it choked
// on. A row reaching outside the module would therefore disclose the content
// of any file the user can read, through `list runnables`, `show runnable` and
// the MCP tool alike.
func TestLoadDerivedOperationsRefusesAPathOutsideTheModule(t *testing.T) {
	outside := t.TempDir()
	const marker = "SECRET-CONTENT-MARKER"
	require.NoError(t, os.WriteFile(filepath.Join(outside, runnables.PackageFileName), []byte(marker), 0o600))

	for _, rowPath := range []string{
		"../" + filepath.Base(outside),
		"contracts/runnables/../../../" + filepath.Base(outside),
		"/etc",
		"./contracts",
		"",
	} {
		moduleDir := t.TempDir()
		writeIndexWithPath(t, moduleDir, rowPath)

		_, err := runnables.LoadDerivedOperations(moduleDir)
		require.Error(t, err, "path %q was accepted", rowPath)
		require.NotContains(t, err.Error(), marker, "path %q leaked file content", rowPath)
	}
}

// Lexical checking is not containment: a row naming an ordinary-looking
// directory that happens to be a symlink still resolves outside the module.
func TestLoadDerivedOperationsRefusesASymlinkOutOfTheModule(t *testing.T) {
	outside := t.TempDir()
	const marker = "SECRET-CONTENT-MARKER"
	require.NoError(t, os.WriteFile(filepath.Join(outside, runnables.PackageFileName), []byte(marker), 0o600))

	moduleDir := t.TempDir()
	writeIndexWithPath(t, moduleDir, "contracts/runnables/escape")
	require.NoError(t, os.Symlink(outside, filepath.Join(moduleDir, "contracts", "runnables", "escape")))

	_, err := runnables.LoadDerivedOperations(moduleDir)
	require.Error(t, err)
	require.NotContains(t, err.Error(), marker)
}

// A listing that cannot say what builds an operation is reporting a broken
// package as a working one, and this surface is documented as strict.
func TestLoadDerivedOperationsFailsOnAnUnreadableAgent(t *testing.T) {
	moduleDir := t.TempDir()
	runnablestest.Write(t, moduleDir, "test-workspace", "documents", "runtime-worker-grpc-apply-text")

	pkgFile := filepath.Join(moduleDir, "contracts", "runnables", "runtime-worker", "grpc", "ApplyText", runnables.PackageFileName)
	data, err := os.ReadFile(pkgFile)
	require.NoError(t, err)
	edited := strings.Replace(string(data), `"kind":"SERVICE"`, `"kind":"UNKNOWN"`, 1)
	require.NotEqual(t, string(data), edited, "the fixture package did not carry the agent kind")
	require.NoError(t, os.WriteFile(pkgFile, []byte(edited), 0o600))

	_, err = runnables.LoadDerivedOperations(moduleDir)
	require.ErrorContains(t, err, "agent")
}

// A derived directory holding packages but no index is a module whose whole
// derived surface would otherwise vanish from every listing behind one deleted
// file.
func TestLoadIndexRefusesAPopulatedTreeWithNoIndex(t *testing.T) {
	moduleDir := t.TempDir()
	runnablestest.Write(t, moduleDir, "test-workspace", "documents", "runtime-worker-grpc-apply-text")
	require.NoError(t, os.Remove(filepath.Join(moduleDir, "contracts", "runnables", runnables.IndexFileName)))

	_, err := runnables.LoadIndex(moduleDir)
	require.ErrorContains(t, err, runnables.IndexFileName)

	// An absent directory still means "nothing derived", not an error.
	index, err := runnables.LoadIndex(t.TempDir())
	require.NoError(t, err)
	require.Nil(t, index)
}
