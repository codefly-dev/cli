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

// findRunnable documents that "an unrelated broken declaration elsewhere in
// the workspace still does not fail this command", and loading every module's
// derived packages up front would break exactly that: a corrupt package
// belonging to some other operation must not fail a question that was never
// about it.
func TestShowRunnableIgnoresABrokenDerivedPackageInAnotherModule(t *testing.T) {
	dir := writeDerivedWorkspace(t, "runtime-worker-grpc-apply-text", false)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"),
		[]byte("name: test-ws\nlayout: modules\nmodules:\n  - name: documents\n  - name: billing\n"), 0o644))

	// billing derives an operation too, and its package is truncated.
	billingDir := filepath.Join(dir, "modules", "billing")
	require.NoError(t, os.MkdirAll(billingDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(billingDir, "module.codefly.yaml"), []byte("kind: module\nname: billing\n"), 0o644))
	runnablestest.Write(t, billingDir, "test-ws", "billing", "billing-grpc-charge")
	broken := filepath.Join(billingDir, "contracts", "runnables", "runtime-worker", "grpc", "ApplyText", "runnable-package.json")
	require.NoError(t, os.WriteFile(broken, []byte("{ this is not a package"), 0o644))

	t.Chdir(dir)
	t.Cleanup(func() { showRunnableJSON, showRunnableVersion = false, "" })

	cmd, buf := newShowTestCmd()
	require.NoError(t, showRunnable(cmd, "runtime-worker-grpc-apply-text"),
		"a broken package in billing failed a question about documents")
	require.Contains(t, buf.String(), "runtime-worker/grpc/ApplyText")

	// The broken one still fails when it is the one being asked about.
	cmd, _ = newShowTestCmd()
	require.Error(t, showRunnable(cmd, "billing-grpc-charge"))
}
