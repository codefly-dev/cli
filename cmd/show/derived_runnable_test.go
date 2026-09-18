package show

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/runnables/runnablestest"
	"github.com/stretchr/testify/require"
)

// writeDerivedWorkspace lays out a module workspace holding one derived
// operation, and optionally an authored runnable of the same name so the
// collision between the two can be exercised.
func writeDerivedWorkspace(t *testing.T, derivedName string, alsoDeclare bool) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"),
		[]byte("name: test-ws\nlayout: modules\nmodules:\n  - name: documents\n"), 0o644))

	moduleDir := filepath.Join(dir, "modules", "documents")
	require.NoError(t, os.MkdirAll(moduleDir, 0o755))
	module := "kind: module\nname: documents\n"
	if alsoDeclare {
		module += "runnables:\n  - name: " + derivedName + "\n"
		runnableDir := filepath.Join(moduleDir, "runnables", derivedName)
		require.NoError(t, os.MkdirAll(runnableDir, 0o755))
		declaration := "kind: runnable\nname: " + derivedName + "\n" + declaredRunnableBody
		require.NoError(t, os.WriteFile(filepath.Join(runnableDir, "runnable.codefly.yaml"), []byte(declaration), 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "module.codefly.yaml"), []byte(module), 0o644))

	runnablestest.Write(t, moduleDir, "test-ws", "documents", derivedName)
	return dir
}

const declaredRunnableBody = `version: 0.1.0
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
  facilities: [native]
  timeout: 2m
  cancellation: none
  recovery: recompute
`

func TestShowRunnableRendersADerivedOperation(t *testing.T) {
	t.Chdir(writeDerivedWorkspace(t, "runtime-worker-grpc-apply-text", false))
	t.Cleanup(func() { showRunnableJSON, showRunnableVersion = false, "" })

	cmd, buf := newShowTestCmd()
	require.NoError(t, showRunnable(cmd, "runtime-worker-grpc-apply-text"))

	out := buf.String()
	require.Contains(t, out, "Runnable:   runtime-worker-grpc-apply-text @ 0.1.0 (derived)")
	require.Contains(t, out, "Source:     runtime-worker/grpc/ApplyText")
	require.Contains(t, out, "Method:     "+runnablestest.Method)
	require.Contains(t, out, "Messages:   documents.ingest.v1.ApplyTextRequest -> documents.ingest.v1.ApplyTextResponse")
	require.Contains(t, out, "facilities:   service")
	require.Contains(t, out, "recovery:     receipt")
	// The policy and authority a binding installs, which the package never
	// carries, are what an operator most needs to see next to the contract.
	require.Contains(t, out, "audience:     documents.ingestion")
	require.Contains(t, out, "invoke:       documents:ingest+read")
	require.Contains(t, out, "lookup:       documents:read")
	require.Contains(t, out, "receipt:      /documents.ingest.v1.IngestionService/LookupText")
	// A derived operation is reached on a service the workspace already runs,
	// so there is no handler and no dependency report to render.
	require.NotContains(t, out, "Handler:")
	require.NotContains(t, out, "Dependencies:")
}

func TestShowRunnableJSONForADerivedOperationCarriesItsSourceAndDigest(t *testing.T) {
	t.Chdir(writeDerivedWorkspace(t, "runtime-worker-grpc-apply-text", false))
	t.Cleanup(func() { showRunnableJSON, showRunnableVersion = false, "" })
	showRunnableJSON = true

	cmd, buf := newShowTestCmd()
	require.NoError(t, showRunnable(cmd, "runtime-worker-grpc-apply-text"))

	var report map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &report))
	require.Equal(t, "runtime-worker/grpc/ApplyText", report["source"])
	require.Equal(t, runnablestest.Method, report["method"])
	require.Equal(t, "documents", report["module"])
	require.NotEmpty(t, report["digest"])
	require.Equal(t, []any{"service"}, report["execution"].(map[string]any)["facilities"])
	operation := report["operation"].(map[string]any)
	require.Equal(t, "documents.ingestion", operation["audience"])
}

// A declared name and a derived name can collide, and answering with either
// one would describe a runnable the caller did not ask for.
func TestShowRunnableRefusesANameThatIsBothDeclaredAndDerived(t *testing.T) {
	t.Chdir(writeDerivedWorkspace(t, "word-count", true))
	t.Cleanup(func() { showRunnableJSON, showRunnableVersion = false, "" })

	cmd, _ := newShowTestCmd()
	err := showRunnable(cmd, "word-count")
	require.ErrorContains(t, err, "both")
	require.ErrorContains(t, err, "runtime-worker/grpc/ApplyText")
}
