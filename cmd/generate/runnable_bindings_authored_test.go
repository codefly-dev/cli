package generate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/environments"
	runnablev0 "github.com/codefly-dev/core/generated/go/codefly/runnable/v0"
	"github.com/codefly-dev/core/resources"
	corerunnable "github.com/codefly-dev/core/runnable"
	"github.com/stretchr/testify/require"
)

// saveAuthoredTool adds an authored runnable to the fixture module: a PDF
// document in, rows out, offered as a read-only tool. operation is its
// declaration's operation block; nil declares none.
func saveAuthoredTool(t *testing.T, ctx context.Context, root, moduleName string, operation map[string]any) {
	t.Helper()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, moduleName)
	require.NoError(t, err)
	agent := &resources.Agent{Kind: resources.RunnableAgent, Name: "python", Version: "0.0.7", Publisher: "codefly.dev"}
	r, err := module.NewRunnable(ctx, "pdf-extract-tables", agent, "handler.py")
	require.NoError(t, err)
	r.Contract.Input.Fields = []*resources.RunnableField{{Name: "pdf", Type: resources.RunnableFieldFile, MediaTypes: []string{"application/pdf"}}}
	r.Contract.Output.Fields = []*resources.RunnableField{{Name: "rows", Type: resources.RunnableFieldArray,
		Items: &resources.RunnableField{Type: resources.RunnableFieldArray, Items: &resources.RunnableField{Type: resources.RunnableFieldString}}}}
	r.Execution.Facilities = []resources.RunnableFacility{resources.RunnableFacilityGenerated}
	r.Execution.Timeout = "30s"
	r.Operation = operation
	require.NoError(t, r.Save(ctx))
}

func pdfToolOperation() map[string]any {
	return map[string]any{
		"audience":      "workbench",
		"invoke_scopes": []any{map[string]any{"resource_kind": "workbench.tools", "actions": []any{"use", "read"}}},
		"lookup_scopes": []any{map[string]any{"resource_kind": "workbench.tools", "actions": []any{"read"}}},
		"tool": map[string]any{
			"name":        "pdf_extract_tables",
			"description": "Extract the tables of a PDF document as rows of cell text.",
			"effect":      "EFFECT_READ_ONLY",
		},
	}
}

// An authored tool is bound beside the derived operations, in the same group
// and as the same prepared value: the harness's local address in the run's
// scope, the Connect procedure the harness answers, the contract with its
// FILE field, and the policy carrying the tool. A caller that installs
// derived operations reads its tool with core's own reader.
func TestRunnableBindingsBindAnAuthoredTool(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0", connectEndpoint())
	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	publishFixtureConnectEndpoint(t, moduleDir, "connect", nil)
	saveAuthoredTool(t, ctx, root, "documents", pdfToolOperation())
	declareLocalNamingScope(t, ctx, root, "alpha")

	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	values := readPreparedValues(t, filepath.Join(root, "configurations", "local", RunnableBindingsGroup+".env"))
	require.Contains(t, values, "DOCUMENTS__PDF_EXTRACT_TABLES")
	require.Len(t, values, 2, "the derived operation is still bound beside the authored one")

	prepared, err := corerunnable.DecodePrepared([]byte(values["DOCUMENTS__PDF_EXTRACT_TABLES"]))
	require.NoError(t, err)
	harness, err := common.ResolveRunnableHarness(ctx, "test-workspace", "documents", "pdf-extract-tables", "alpha")
	require.NoError(t, err)
	require.Equal(t, harness.Address, prepared.GetCall().GetAddress(), "the address `codefly run runnable` serves in the same scope")
	require.Equal(t, corerunnable.ServedInvokeProcedure, prepared.GetCall().GetConnect().GetProcedure())
	require.Equal(t, []string{"application/pdf"}, prepared.GetContract().GetInput().GetFields()[0].GetMediaTypes())
	tool, err := corerunnable.ToolFromPrepared(prepared)
	require.NoError(t, err)
	require.Equal(t, "pdf_extract_tables", tool.GetName())
	require.Equal(t, runnablev0.ToolExposure_EFFECT_READ_ONLY, tool.GetEffect())

	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	env, err := environments.Select(workspace, "local")
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "documents")
	require.NoError(t, err)
	r, err := module.LoadRunnableFromName(ctx, "pdf-extract-tables")
	require.NoError(t, err)
	key, value, err := PrepareAuthoredBinding(ctx, workspace, env, module, r)
	require.NoError(t, err)
	require.Equal(t, "DOCUMENTS__PDF_EXTRACT_TABLES", key)
	require.Equal(t, values[key], value, "`codefly run runnable` shows exactly the value generation writes")

	resetRunnableBindingsFlags(t)
	runnableBindingsCheck = true
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
}

// A runnable with no operation block is not bound to anyone, and one whose
// block does not validate fails generation by name rather than being skipped.
func TestRunnableBindingsHoldAnAuthoredOperationToItsDeclaration(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0", connectEndpoint())
	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	publishFixtureConnectEndpoint(t, moduleDir, "connect", nil)
	saveAuthoredTool(t, ctx, root, "documents", nil)

	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	values := readPreparedValues(t, filepath.Join(root, "configurations", "local", RunnableBindingsGroup+".env"))
	require.NotContains(t, values, "DOCUMENTS__PDF_EXTRACT_TABLES")

	declaration := filepath.Join(moduleDir, "runnables", "pdf-extract-tables", resources.RunnableConfigurationName)
	raw, err := os.ReadFile(declaration)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(declaration, append(raw, []byte("operation:\n  audience: workbench\n  total_timeout: 10s\n")...), 0o600))
	resetRunnableBindingsFlags(t)
	err = RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil)
	require.ErrorContains(t, err, "runnable documents/pdf-extract-tables")
	require.ErrorContains(t, err, "total_timeout, which its execution block owns")
}
