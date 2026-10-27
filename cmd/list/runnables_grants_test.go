package list

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
	"github.com/codefly-dev/cli/pkg/runnables/runnablestest"
	"github.com/stretchr/testify/require"
)

// toolKey is the configuration key the fixture operation's authority is
// answered under: the module and the derived name, as
// generate.RunnableBindingKey spells them.
const toolKey = "DOCUMENTS__RUNTIME_WORKER_GRPC_APPLY_TEXT"

// writeWorkspaceWithAToolAndSlot lays out a module holding one derived
// operation that its owner exposes as a tool and whose authority it cannot
// spell alone: one required scope slot the composition must answer. selections
// is the environment's runnable-scope-selections block, indented to sit under
// the environment, or empty for an environment that answers nothing.
func writeWorkspaceWithAToolAndSlot(t *testing.T, selections string) string {
	t.Helper()
	dir := t.TempDir()
	workspace := "name: test-workspace\nlayout: modules\nmodules:\n  - name: documents\nenvironments:\n  - name: staging\n" + selections
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"), []byte(workspace), 0o644))

	moduleDir := filepath.Join(dir, "modules", "documents")
	require.NoError(t, os.MkdirAll(moduleDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "module.codefly.yaml"),
		[]byte("kind: module\nname: documents\n"), 0o644))
	runnablestest.Write(t, moduleDir, "test-workspace", "documents", "runtime-worker-grpc-apply-text")

	// The shared fixture writes an operation with no exposure and no slot,
	// which is the common case; this one is the case a composition has to
	// answer, so its operation document is rewritten rather than the helper
	// changed for every other test.
	operation := map[string]any{
		"method":          runnablestest.Method,
		"attempt_timeout": "10s",
		"total_timeout":   "1m0s",
		"max_attempts":    1,
		"backoff":         "1s",
		"audience":        "documents.ingestion",
		"invoke_scopes":   []map[string]any{{"resource_kind": "documents", "actions": []string{"ingest", "read"}}},
		"lookup_scopes":   []map[string]any{{"resource_kind": "documents", "actions": []string{"read"}}},
		"lookup_method":   "/documents.ingest.v1.IngestionService/LookupText",
		"completion":      "COMPLETION_CALL",
		"tool":            map[string]any{"name": "apply_text", "description": "Apply text to a document.", "effect": "EFFECT_MUTATION"},
		"required_scope_slots": []map[string]any{{
			"name": "collection", "required_actions": []string{"write"},
		}},
	}
	document, err := json.MarshalIndent(operation, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(moduleDir, "contracts", "runnables", "runtime-worker", "grpc", "ApplyText", runnablespkg.OperationFileName),
		append(document, '\n'), 0o644))
	return dir
}

// listOneDerived runs the listing over the fixture and returns its single
// derived row. Reading the JSON rather than the table is deliberate: the row a
// console or a picker consumes is the one under test.
func listOneDerived(t *testing.T, dir string) runnablespkg.Identity {
	t.Helper()
	t.Chdir(dir)
	t.Cleanup(func() {
		listRunnablesJSON, listRunnablesModule, listRunnablesEnv, listRunnablesTools = false, "", "", false
	})
	listRunnablesJSON, listRunnablesEnv = true, "staging"
	cmd, buf := newListTestCmd()
	require.NoError(t, listRunnables(cmd))

	var entries []runnablespkg.Identity
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entries))
	require.Len(t, entries, 1, "the fixture module derives exactly one operation")
	return entries[0]
}

// A listing is what a composition picks its tools from, so it carries the
// owner's exposure as the owner declared it: the model-facing name and the
// explicit effect. Nothing is defaulted — an operation with no exposure is not
// a tool.
func TestAListingCarriesTheToolItsOwnerExposed(t *testing.T) {
	entry := listOneDerived(t, writeWorkspaceWithAToolAndSlot(t, ""))

	require.NotNil(t, entry.Tool, "an exposed operation is listed as the tool it is")
	require.Equal(t, "apply_text", entry.Tool.Name)
	require.Equal(t, "EFFECT_MUTATION", entry.Tool.Effect, "the effect is the owner's, never inferred")
	require.Equal(t, "Apply text to a document.", entry.Tool.Description)
}

// An operation nobody has answered is the signal that matters on a version
// bump: a module that adds an operation adds a row its environment has not
// granted, and `generate runnable-bindings` refuses it by name rather than
// preparing a binding with the owner's question still open.
func TestAnUnansweredSlotIsReportedAsNeedingScopes(t *testing.T) {
	entry := listOneDerived(t, writeWorkspaceWithAToolAndSlot(t, ""))

	require.NotNil(t, entry.Grant)
	require.Equal(t, toolKey, entry.Grant.Key, "the key is where the answer is written")
	require.Equal(t, runnablespkg.GrantNeedsScopes, entry.Grant.State)
	require.Equal(t, []string{"collection"}, entry.Grant.Slots, "the open question is named")
}

// An environment that answers the slot grants the tool.
func TestAnAnsweredSlotIsReportedAsSelected(t *testing.T) {
	answered := "    runnable-scope-selections:\n" +
		"      " + toolKey + ":\n" +
		"        - slot: collection\n" +
		"          invoke:\n" +
		"            - resource_kind: documents.collection\n" +
		"              actions: [write]\n" +
		"              resource_ids: [acme]\n"
	entry := listOneDerived(t, writeWorkspaceWithAToolAndSlot(t, answered))

	require.NotNil(t, entry.Grant)
	require.Equal(t, runnablespkg.GrantSelected, entry.Grant.State)
}

// An environment that declines it with an empty list grants nothing, and the
// listing says so rather than showing a tool the composition will not install.
func TestADeclinedOperationIsReportedAsDeclined(t *testing.T) {
	declined := "    runnable-scope-selections:\n      " + toolKey + ": []\n"
	entry := listOneDerived(t, writeWorkspaceWithAToolAndSlot(t, declined))

	require.NotNil(t, entry.Grant)
	require.Equal(t, runnablespkg.GrantDeclined, entry.Grant.State)
}

// Without --env there is no grant to report: a listing never implies an answer
// no environment gave.
func TestWithoutAnEnvironmentNoGrantIsReported(t *testing.T) {
	dir := writeWorkspaceWithAToolAndSlot(t, "")
	t.Chdir(dir)
	t.Cleanup(func() {
		listRunnablesJSON, listRunnablesModule, listRunnablesEnv, listRunnablesTools = false, "", "", false
	})
	listRunnablesJSON = true
	cmd, buf := newListTestCmd()
	require.NoError(t, listRunnables(cmd))

	var entries []runnablespkg.Identity
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entries))
	require.Len(t, entries, 1)
	require.Nil(t, entries[0].Grant)
	require.NotNil(t, entries[0].Tool, "the exposure is the owner's fact and does not depend on an environment")
}

// An environment the workspace does not declare is an error, not a listing
// whose grant column is quietly absent.
func TestAnUndeclaredEnvironmentIsRefused(t *testing.T) {
	t.Chdir(writeWorkspaceWithAToolAndSlot(t, ""))
	t.Cleanup(func() {
		listRunnablesJSON, listRunnablesModule, listRunnablesEnv, listRunnablesTools = false, "", "", false
	})
	listRunnablesJSON, listRunnablesEnv = true, "production"
	cmd, _ := newListTestCmd()
	require.ErrorContains(t, listRunnables(cmd), "production")
}

// --tools lists only what a composition can actually grant as a tool, so a
// picker is not offered operations their owner never exposed.
func TestOnlyExposedOperationsAreListedAsTools(t *testing.T) {
	dir := writeWorkspaceWithADerivedOperation(t) // the shared fixture: no exposure
	t.Chdir(dir)
	t.Cleanup(func() {
		listRunnablesJSON, listRunnablesModule, listRunnablesEnv, listRunnablesTools = false, "", "", false
	})
	listRunnablesJSON, listRunnablesTools = true, true
	cmd, buf := newListTestCmd()
	require.NoError(t, listRunnables(cmd))

	var entries []runnablespkg.Identity
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entries))
	require.Empty(t, entries, "an operation with no exposure is not a tool, and an authored runnable is not one either")
}
