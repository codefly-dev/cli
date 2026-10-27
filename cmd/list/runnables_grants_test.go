package list

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
func listOneDerived(t *testing.T, dir string) runnableRow {
	listing := listEnvironment(t, dir)
	require.Len(t, listing.Runnables, 1, "the fixture module derives exactly one operation")
	return listing.Runnables[0]
}

// listEnvironment runs the listing against the fixture's staging environment
// and returns what a console or a picker consumes: the rows, and the answers
// that match no operation.
func listEnvironment(t *testing.T, dir string) runnableListing {
	t.Helper()
	t.Chdir(dir)
	t.Cleanup(func() {
		listRunnablesJSON, listRunnablesModule, listRunnablesEnv, listRunnablesTools = false, "", "", false
	})
	listRunnablesJSON, listRunnablesEnv = true, "staging"
	cmd, buf := newListTestCmd()
	require.NoError(t, listRunnables(cmd))

	var listing runnableListing
	require.NoError(t, json.Unmarshal(buf.Bytes(), &listing))
	return listing
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

	var entries []runnableRow
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

	var entries []runnableRow
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entries))
	require.Empty(t, entries, "an operation with no exposure is not a tool, and an authored runnable is not one either")
}

// writeTwoSlotOperation is the case that exposed the listing's first shape as
// wrong: an operation whose owner asks TWO questions. An environment that
// answers one of them has answered something, and the generator still refuses
// the run.
func writeTwoSlotOperation(t *testing.T, selections string) string {
	t.Helper()
	dir := writeWorkspaceWithAToolAndSlot(t, selections)
	operation := operationDocument(t, dir)
	operation["required_scope_slots"] = []map[string]any{
		{"name": "collection", "required_actions": []string{"write"}},
		{"name": "source", "required_actions": []string{"read"}},
	}
	writeOperationDocument(t, dir, operation)
	return dir
}

func operationDocument(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(operationPath(dir))
	require.NoError(t, err)
	document := map[string]any{}
	require.NoError(t, json.Unmarshal(data, &document))
	return document
}

func writeOperationDocument(t *testing.T, dir string, document map[string]any) {
	t.Helper()
	data, err := json.MarshalIndent(document, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(operationPath(dir), append(data, '\n'), 0o644))
}

func operationPath(dir string) string {
	return filepath.Join(dir, "modules", "documents", "contracts", "runnables",
		"runtime-worker", "grpc", "ApplyText", runnablespkg.OperationFileName)
}

// collectionOnly answers the `collection` slot and nothing else.
const collectionOnly = "    runnable-scope-selections:\n" +
	"      " + toolKey + ":\n" +
	"        - slot: collection\n" +
	"          invoke:\n" +
	"            - resource_kind: documents.collection\n" +
	"              actions: [write]\n" +
	"              resource_ids: [acme]\n"

// A selection that answers some of the owner's questions is not a grant. The
// listing's first shape read any non-empty selection as `selected`, so an
// operation declaring two slots and answered for one reported as granted while
// `generate runnable-bindings` refused the run — wrong in the one place an
// operator looks to check their work.
func TestAPartiallyAnsweredOperationIsNotSelected(t *testing.T) {
	entry := listOneDerived(t, writeTwoSlotOperation(t, collectionOnly))

	require.NotNil(t, entry.Grant)
	require.Equal(t, runnablespkg.GrantInvalid, entry.Grant.State,
		"an answer generation refuses is not a grant")
	require.Contains(t, entry.Grant.Reason, "source", "the unanswered slot is named")
	require.Equal(t, []string{"collection", "source"}, entry.Grant.Slots)
}

// A selection naming a slot the operation does not declare is the same class
// of mistake, and core refuses it with its own words.
func TestASelectionNamingAnUndeclaredSlotIsInvalid(t *testing.T) {
	wrong := "    runnable-scope-selections:\n" +
		"      " + toolKey + ":\n" +
		"        - slot: colection\n" +
		"          invoke:\n" +
		"            - resource_kind: documents.collection\n" +
		"              actions: [write]\n" +
		"              resource_ids: [acme]\n"
	entry := listOneDerived(t, writeWorkspaceWithAToolAndSlot(t, wrong))

	require.NotNil(t, entry.Grant)
	require.Equal(t, runnablespkg.GrantInvalid, entry.Grant.State)
	require.Contains(t, entry.Grant.Reason, "colection")
}

// Declining is only something an operation with a required scope slot can be
// answered with: generate's declinableOperation refuses `[]` against an
// operation that has nothing to decline, so reporting it as a valid `declined`
// would legitimise a configuration that fails the run.
func TestDecliningAnOperationWithNoSlotIsInvalid(t *testing.T) {
	dir := writeWorkspaceWithAToolAndSlot(t, "    runnable-scope-selections:\n      "+toolKey+": []\n")
	operation := operationDocument(t, dir)
	delete(operation, "required_scope_slots")
	writeOperationDocument(t, dir, operation)

	entry := listOneDerived(t, dir)
	require.NotNil(t, entry.Grant)
	require.Equal(t, runnablespkg.GrantInvalid, entry.Grant.State)
	require.Contains(t, entry.Grant.Reason, "nothing to decline")
}

// An answer that names no operation is the likeliest mistake with a key that
// is typed by hand, and the listing used to drop it: the real operation read
// as unanswered and nothing pointed at the typo, while the generator would
// have named it.
func TestAnAnswerThatNamesNoOperationIsReported(t *testing.T) {
	typo := "    runnable-scope-selections:\n      " + toolKey[:len(toolKey)-1] + ": []\n"
	listing := listEnvironment(t, writeWorkspaceWithAToolAndSlot(t, typo))

	require.Equal(t, []string{toolKey[:len(toolKey)-1]}, listing.UnansweredKeys,
		"an answer to no question is reported, not dropped")
	require.Len(t, listing.Runnables, 1)
	require.Equal(t, runnablespkg.GrantNeedsScopes, listing.Runnables[0].Grant.State,
		"and the operation it meant to answer still reads as unanswered")
}

// Without --env the listing stays the array it has always been, so a reader of
// `list runnables --json` is not handed a different shape by this change.
func TestWithoutAnEnvironmentTheJSONStaysAnArray(t *testing.T) {
	t.Chdir(writeWorkspaceWithAToolAndSlot(t, ""))
	t.Cleanup(func() {
		listRunnablesJSON, listRunnablesModule, listRunnablesEnv, listRunnablesTools = false, "", "", false
	})
	listRunnablesJSON = true
	cmd, buf := newListTestCmd()
	require.NoError(t, listRunnables(cmd))
	require.Equal(t, byte('['), buf.Bytes()[0], "the no-environment listing is an array")
}

// The default table is the one every existing reader parses: the tool columns
// appear when a caller asks about tools or about an environment, never before.
func TestTheDefaultTableKeepsItsColumns(t *testing.T) {
	t.Chdir(writeWorkspaceWithAToolAndSlot(t, ""))
	t.Cleanup(func() {
		listRunnablesJSON, listRunnablesModule, listRunnablesEnv, listRunnablesTools = false, "", "", false
	})
	cmd, buf := newListTestCmd()
	require.NoError(t, listRunnables(cmd))
	header := strings.Split(buf.String(), "\n")[0]
	require.NotContains(t, header, "TOOL")
	require.NotContains(t, header, "EFFECT")

	listRunnablesTools = true
	cmd, buf = newListTestCmd()
	require.NoError(t, listRunnables(cmd))
	require.Contains(t, strings.Split(buf.String(), "\n")[0], "TOOL")
}
