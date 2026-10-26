package generate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

// declineFixture derives three operations of one module: a slot-only one, one
// with a slot and fixed scopes, and one with fixed scopes only. It returns the
// workspace root and each operation's binding key by method suffix.
func declineFixture(t *testing.T) (string, map[string]string) {
	t.Helper()
	file := ingestionFile(slotOperation(false),
		markedMethod("SlotAndFixed", ".documents.ingest.v1.ApplyTextRequest", ".documents.ingest.v1.ApplyTextResponse", slotOperation(true)),
		markedMethod("FixedOnly", ".documents.ingest.v1.ApplyTextRequest", ".documents.ingest.v1.ApplyTextResponse", conformingOperation()))
	root, moduleDir := saveConnectRunnableFixture(t, context.Background(), descriptorSet(t, file), "0.1.0")
	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	derived, err := runnablespkg.LoadDerivedOperations(moduleDir)
	require.NoError(t, err)
	require.Len(t, derived, 3)
	keys := map[string]string{}
	for _, operation := range derived {
		method := operation.Entry.Method[strings.LastIndex(operation.Entry.Method, "/")+1:]
		keys[method] = RunnableBindingKey("documents", operation.Entry.Name)
	}
	require.Contains(t, keys, "SlotAndFixed")
	require.Contains(t, keys, "FixedOnly")
	require.Len(t, keys, 3)
	return root, keys
}

func slotOnlyKey(keys map[string]string) string {
	for method, key := range keys {
		if method != "SlotAndFixed" && method != "FixedOnly" {
			return key
		}
	}
	return ""
}

// A base composition holds no resource IDs for a slot whose resources are
// created at runtime, so it answers the slot with []: that operation is not
// prepared and receives no authority, and every other operation still is.
func TestDeclinedBindingIsAbsentAndEveryOtherOperationPrepares(t *testing.T) {
	root, keys := declineFixture(t)
	declined := slotOnlyKey(keys)
	writeSelectionEnvironment(t, root, map[string]environments.ScopeSelections{
		declined:             {},
		keys["SlotAndFixed"]: {sourceSelection()},
	})
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	target := filepath.Join(root, "configurations", "selected", RunnableBindingsGroup+".env")
	values := readPreparedValues(t, target)
	require.NotContains(t, values, declined, "a declined operation has no prepared binding")
	require.Len(t, values, 2)
	slotted := verifyResolvedBinding(t, values[keys["SlotAndFixed"]], slotOperation(true).Tool)
	require.Len(t, slotted.Policy.InvokeScopes, 2)
	require.True(t, proto.Equal(sourceSelection().Invoke[0], slotted.Policy.InvokeScopes[1]))
	fixed := verifyResolvedBinding(t, values[keys["FixedOnly"]], nil)
	require.True(t, proto.Equal(conformingOperation().InvokeScopes[0], fixed.Policy.InvokeScopes[0]))
	raw, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Contains(t, string(raw), "# "+declined+": declined (runnable-scope-selections: [])",
		"the file names the decline, so a consumer refused for the missing key finds why")
	require.NotContains(t, string(raw), declined+"=")

	// Declining every operation that has a slot still writes the file: it
	// carries the remaining binding and the record of each decline.
	writeSelectionEnvironment(t, root, map[string]environments.ScopeSelections{declined: {}, keys["SlotAndFixed"]: {}})
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	values = readPreparedValues(t, target)
	require.Equal(t, []string{keys["FixedOnly"]}, mapKeys(values))
}

// --check sees a decline added and a decline removed: the bytes differ both
// ways, and a stale file is never rewritten by --check.
func TestRunnableBindingsCheckDetectsADeclineAddedOrRemoved(t *testing.T) {
	root, keys := declineFixture(t)
	declined := slotOnlyKey(keys)
	target := filepath.Join(root, "configurations", "selected", RunnableBindingsGroup+".env")
	selected := map[string]environments.ScopeSelections{declined: {sourceSelection()}, keys["SlotAndFixed"]: {sourceSelection()}}
	declining := map[string]environments.ScopeSelections{declined: {}, keys["SlotAndFixed"]: {sourceSelection()}}

	writeSelectionEnvironment(t, root, selected)
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	before, err := os.ReadFile(target)
	require.NoError(t, err)

	runnableBindingsCheck = true
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	writeSelectionEnvironment(t, root, declining)
	require.ErrorContains(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil), "out of date", "adding a decline is drift")
	after, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, before, after, "--check must not write")

	runnableBindingsCheck = false
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	runnableBindingsCheck = true
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil), "the declining file is current")
	writeSelectionEnvironment(t, root, selected)
	require.ErrorContains(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil), "out of date", "removing a decline is drift")
}

// An absent key is not a decline: the slot is still refused exactly as it was,
// so nothing is skipped by omission, and nothing is written.
func TestAbsentSelectionKeyIsStillRefused(t *testing.T) {
	root, keys := declineFixture(t)
	writeSelectionEnvironment(t, root, map[string]environments.ScopeSelections{keys["SlotAndFixed"]: {sourceSelection()}})
	err := RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil)
	require.ErrorContains(t, err, `required scope slot "source" but no ScopeSelection was supplied`)
	require.ErrorContains(t, err, "runnable-scope-selections["+slotOnlyKey(keys)+"]")
	require.NoFileExists(t, filepath.Join(root, "configurations", "selected", RunnableBindingsGroup+".env"))
}

// [] on an operation with no required slot declines nothing and would hide a
// typo; [] on a key no operation derives is an unknown key like any other.
func TestEmptySelectionListRefusals(t *testing.T) {
	for _, tt := range []struct {
		name string
		key  func(map[string]string) string
		want []string
	}{
		{"slot-less operation", func(keys map[string]string) string { return keys["FixedOnly"] }, []string{"declares no required scope slot", "nothing to decline"}},
		{"unknown key", func(map[string]string) string { return "DOCUMENTS__TYPO" }, []string{"runnable-scope-selections[DOCUMENTS__TYPO] names no derived operation"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, keys := declineFixture(t)
			writeSelectionEnvironment(t, root, map[string]environments.ScopeSelections{
				slotOnlyKey(keys):    {sourceSelection()},
				keys["SlotAndFixed"]: {sourceSelection()},
				tt.key(keys):         {},
			})
			err := RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil)
			for _, want := range tt.want {
				require.ErrorContains(t, err, want)
			}
			require.NoFileExists(t, filepath.Join(root, "configurations", "selected", RunnableBindingsGroup+".env"))
		})
	}
}

// With no derived operation and nothing declined the command writes no file,
// so --check holds the workspace to that absence rather than to a header.
func TestRunnableBindingsCheckWithNothingToBind(t *testing.T) {
	root, _ := saveConnectRunnableFixture(t, context.Background(), descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0")
	t.Chdir(root)
	resetRunnableBindingsFlags(t)
	writeSelectionEnvironment(t, root, nil)
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	target := filepath.Join(root, "configurations", "selected", RunnableBindingsGroup+".env")
	require.NoFileExists(t, target)
	runnableBindingsCheck = true
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	require.NoError(t, os.WriteFile(target, []byte(runnableBindingsFileHeader), 0o600))
	require.ErrorContains(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil), "out of date", "a stale file where none belongs is drift")
}

func mapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

// The file belongs to a configuration profile, which several environments may
// share, so the decline comment names no environment: two environments that
// resolve the same addresses and decline the same key write the same bytes,
// and --check for one passes right after generating for the other.
func TestDeclineCommentIsTheSameForEveryEnvironmentOfAProfile(t *testing.T) {
	root, keys := declineFixture(t)
	declined := slotOnlyKey(keys)
	selections := map[string]environments.ScopeSelections{declined: {}, keys["SlotAndFixed"]: {sourceSelection()}}
	file := filepath.Join(root, "workspace.codefly.yaml")
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	var workspace map[string]any
	require.NoError(t, yaml.Unmarshal(data, &workspace))
	workspace["environments"] = []*environments.Environment{
		{Name: "local", ConfigurationProfile: "selected", RunnableScopeSelections: selections},
		// Both names are local, so both resolve the same addresses: only the
		// decline comment could tell their files apart.
		{Name: "local-fixture", ConfigurationProfile: "selected", RunnableScopeSelections: selections},
	}
	data, err = yaml.Marshal(workspace)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, data, 0o600))

	runnableBindingsEnv = "local"
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	raw, err := os.ReadFile(filepath.Join(root, "configurations", "selected", RunnableBindingsGroup+".env"))
	require.NoError(t, err)
	require.Contains(t, string(raw), "# "+declined+": declined (runnable-scope-selections: []); no binding, no authority\n")
	require.NotContains(t, string(raw), "environment local", "the decline comment names no environment")

	runnableBindingsEnv, runnableBindingsCheck = "local-fixture", true
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil),
		"an environment sharing the profile finds the file current")
}
