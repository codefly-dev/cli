package generate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runnablev0 "github.com/codefly-dev/core/generated/go/codefly/runnable/v0"
	corerunnable "github.com/codefly-dev/core/runnable"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"gopkg.in/yaml.v3"
)

func slotOperation(fixed bool) *runnablev0.Operation {
	declared := conformingOperation()
	if !fixed {
		declared.InvokeScopes, declared.LookupScopes = nil, nil
	}
	declared.RequiredScopeSlots = []*runnablev0.ScopeSlot{{Name: "source", RequiredActions: []string{"invoke", "read"}, Lookup: true}}
	declared.Tool = &runnablev0.ToolExposure{Name: "invoke_source", Description: "Invoke a selected source.", Effect: runnablev0.ToolExposure_EFFECT_MUTATION}
	return declared
}

func sourceSelection() *runnablev0.ScopeSelection {
	return &runnablev0.ScopeSelection{
		Slot:   "source",
		Invoke: []*basev0.WorkScopeV1{{ResourceKind: "datasource.sources", Actions: []string{"invoke", "read"}, ResourceIds: []string{"source-a"}}},
		Lookup: []*basev0.WorkScopeV1{{ResourceKind: "datasource.sources", Actions: []string{"read"}, ResourceIds: []string{"source-a"}}},
	}
}

// Schema reflection is the field list, not a list maintained beside the
// conversions. A new Core field must be populated in this fixture, written
// to operation.json and read back, or this test fails before we can erase it.
func TestOperationDocumentRoundTripsEveryCoreField(t *testing.T) {
	declared := slotOperation(true)
	declared.MaxInputBytes, declared.MaxOutputBytes = 1024, 2048
	declared.InvokeScopes[0].ResourceIds = []string{"document-a"}
	declared.LookupScopes[0].ResourceIds = []string{"document-a"}
	root, moduleDir := saveConnectRunnableFixture(t, context.Background(), descriptorSet(t, ingestionFile(declared)), "0.1.0")
	t.Chdir(root)
	resetRunnablesFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	derived, err := runnablespkg.LoadDerivedOperations(moduleDir)
	require.NoError(t, err)
	require.Len(t, derived, 1)
	document := derived[0].Operation
	data, err := json.Marshal(document)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &fields))
	requireEveryMessageFieldPopulated(t, declared.ProtoReflect())
	require.Contains(t, string(data), `"effect":"EFFECT_MUTATION"`)
	message := declared.ProtoReflect()
	coreFields := message.Descriptor().Fields()
	for i := 0; i < coreFields.Len(); i++ {
		field := coreFields.Get(i)
		require.Truef(t, message.Has(field), "populate Core Operation.%s in the fixture", field.Name())
		require.Contains(t, fields, string(field.Name()), "carry every Core field through the document")
	}
	require.Len(t, fields, coreFields.Len()+1, "only method is document-specific")
	spec, err := operationSpec(document, document.Method, corerunnable.GRPCStatusNames)
	require.NoError(t, err)
	require.True(t, proto.Equal(declared, spec.Policy()), "declared %v; read %v", declared, spec.Policy())
	// Check the Go shape too, so an omitempty zero cannot hide a new field.
	require.Equal(t, coreFields.Len()+1, reflect.TypeFor[runnablespkg.Operation]().NumField())
}

// Descend into every message, including repeated scopes and slots. A future
// nested field must be populated before proto.Equal can prove it survived.
func requireEveryMessageFieldPopulated(t *testing.T, message protoreflect.Message) {
	t.Helper()
	fields := message.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		require.Truef(t, message.Has(field), "populate %s in the round-trip fixture", field.FullName())
		if field.Message() == nil {
			continue
		}
		// Well-known durations have a scalar JSON spelling. Their seconds/nanos
		// representation is protobuf-owned, rather than a document field list.
		if field.Message().FullName() == "google.protobuf.Duration" {
			continue
		}
		if field.IsList() {
			list := message.Get(field).List()
			for j := 0; j < list.Len(); j++ {
				requireEveryMessageFieldPopulated(t, list.Get(j).Message())
			}
		} else {
			requireEveryMessageFieldPopulated(t, message.Get(field).Message())
		}
	}
}

func TestOperationDocumentRefusesUnknownNestedCoreFields(t *testing.T) {
	for _, data := range []string{
		`{"tool":{"name":"invoke_source","new_authority":true}}`,
		`{"required_scope_slots":[{"name":"source","new_authority":true}]}`,
	} {
		var document runnablespkg.Operation
		require.ErrorContains(t, json.Unmarshal([]byte(data), &document), "unknown field")
	}
}

func writeSelectionEnvironment(t *testing.T, root string, selections map[string]environments.ScopeSelections) {
	t.Helper()
	file := filepath.Join(root, "workspace.codefly.yaml")
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	var workspace map[string]any
	require.NoError(t, yaml.Unmarshal(data, &workspace))
	workspace["environments"] = []*environments.Environment{{Name: "local", ConfigurationProfile: "selected", RunnableScopeSelections: selections}}
	data, err = yaml.Marshal(workspace)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, data, 0o600))
}

func readPreparedValues(t *testing.T, target string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(target)
	require.NoError(t, err)
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		require.True(t, ok)
		values[key] = value
	}
	return values
}

func verifyResolvedBinding(t *testing.T, value string, tool *runnablev0.ToolExposure) *runnablev0.PreparedBinding {
	t.Helper()
	prepared, err := corerunnable.DecodePrepared([]byte(value))
	require.NoError(t, err)
	require.NoError(t, corerunnable.VerifyPrepared(prepared))
	require.Empty(t, prepared.Policy.RequiredScopeSlots)
	exposure, err := corerunnable.ToolFromPrepared(prepared)
	if tool == nil {
		require.ErrorIs(t, err, corerunnable.ErrNotATool)
	} else {
		require.NoError(t, err)
		require.True(t, proto.Equal(tool, exposure))
	}
	// The host receipt and the caller binding consume the SAME resolution.
	encoded, err := corerunnable.EncodeResolvedPolicy(&runnablev0.ResolvedPolicy{Operation: prepared.Operation, Policy: prepared.Policy})
	require.NoError(t, err)
	receipt, err := corerunnable.DecodeResolvedPolicy(encoded)
	require.NoError(t, err)
	require.NoError(t, corerunnable.VerifyResolvedPolicy(receipt))
	require.NoError(t, corerunnable.BindingMatchesResolvedPolicy(prepared, receipt))
	return prepared
}

func TestGenerateRunnableBindingsResolvesThreeOperationFixtures(t *testing.T) {
	fixed := conformingOperation()
	both := slotOperation(true)
	file := ingestionFile(slotOperation(false),
		markedMethod("SlotAndFixed", ".documents.ingest.v1.ApplyTextRequest", ".documents.ingest.v1.ApplyTextResponse", both),
		markedMethod("FixedOnly", ".documents.ingest.v1.ApplyTextRequest", ".documents.ingest.v1.ApplyTextResponse", fixed))
	root, moduleDir := saveConnectRunnableFixture(t, context.Background(), descriptorSet(t, file), "0.1.0")
	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	derived, err := runnablespkg.LoadDerivedOperations(moduleDir)
	require.NoError(t, err)
	require.Len(t, derived, 3)
	selections := map[string]environments.ScopeSelections{}
	for _, operation := range derived {
		if !strings.HasSuffix(operation.Entry.Method, "/FixedOnly") {
			require.Len(t, operation.Operation.RequiredScopeSlots, 1)
			selections[RunnableBindingKey("documents", operation.Entry.Name)] = environments.ScopeSelections{sourceSelection()}
		} else {
			require.Empty(t, operation.Operation.RequiredScopeSlots)
		}
	}
	writeSelectionEnvironment(t, root, selections)
	// Parse the registered flags, including --env, not just package globals.
	require.NoError(t, RunnableBindingsCmd.ParseFlags([]string{"--env", "local"}))
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	target := filepath.Join(root, "configurations", "selected", RunnableBindingsGroup+".env")
	values := readPreparedValues(t, target)
	require.Len(t, values, 3)
	for _, operation := range derived {
		key := RunnableBindingKey("documents", operation.Entry.Name)
		prepared := verifyResolvedBinding(t, values[key], operation.Operation.Tool)
		switch {
		case strings.HasSuffix(operation.Entry.Method, "/FixedOnly"):
			// The old path wrote the declared policy directly. Its canonical
			// output stays byte-identical, not just semantically equivalent.
			baseline := proto.CloneOf(prepared)
			baseline.Policy = fixed
			before, err := corerunnable.EncodePrepared(baseline)
			require.NoError(t, err)
			require.Equal(t, string(before), values[key])
		case strings.HasSuffix(operation.Entry.Method, "/SlotAndFixed"):
			require.Len(t, prepared.Policy.InvokeScopes, 2)
			require.True(t, proto.Equal(both.InvokeScopes[0], prepared.Policy.InvokeScopes[0]))
			require.True(t, proto.Equal(both.LookupScopes[0], prepared.Policy.LookupScopes[0]))
			require.True(t, proto.Equal(sourceSelection().Invoke[0], prepared.Policy.InvokeScopes[1]))
		default:
			require.Len(t, prepared.Policy.InvokeScopes, 1)
			require.True(t, proto.Equal(sourceSelection().Invoke[0], prepared.Policy.InvokeScopes[0]))
			require.True(t, proto.Equal(sourceSelection().Lookup[0], prepared.Policy.LookupScopes[0]))
		}
	}
	runnablesCheck = true
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	require.NoError(t, RunnableBindingsCmd.ParseFlags([]string{"--check"}))
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	for _, selection := range selections {
		selection[0].Invoke[0].ResourceIds = []string{"source-b"}
		selection[0].Lookup[0].ResourceIds = []string{"source-b"}
	}
	writeSelectionEnvironment(t, root, selections)
	require.ErrorContains(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil), "out of date")
	require.Equal(t, values, readPreparedValues(t, target), "--check must not write")
}

func TestRunnableScopeSelectionRefusals(t *testing.T) {
	tests := []struct {
		name      string
		operation *runnablev0.Operation
		change    func(*runnablev0.Operation, *runnablev0.ScopeSelection) environments.ScopeSelections
		want      string
	}{
		{"slot-only missing", slotOperation(false), func(_ *runnablev0.Operation, _ *runnablev0.ScopeSelection) environments.ScopeSelections { return nil }, `required scope slot "source" but no ScopeSelection was supplied`},
		{"slot-plus-fixed missing", slotOperation(true), func(_ *runnablev0.Operation, _ *runnablev0.ScopeSelection) environments.ScopeSelections { return nil }, `required scope slot "source" but no ScopeSelection was supplied`},
		{"wildcard id", slotOperation(false), func(_ *runnablev0.Operation, s *runnablev0.ScopeSelection) environments.ScopeSelections {
			s.Invoke[0].ResourceIds = []string{"*"}
			return environments.ScopeSelections{s}
		}, "not an exact id"},
		{"wildcard omission", slotOperation(false), func(_ *runnablev0.Operation, s *runnablev0.ScopeSelection) environments.ScopeSelections {
			s.Invoke[0].ResourceIds = nil
			return environments.ScopeSelections{s}
		}, "never a wildcard"},
		{"wildcard kind", slotOperation(false), func(_ *runnablev0.Operation, s *runnablev0.ScopeSelection) environments.ScopeSelections {
			s.Invoke[0].ResourceKind = "*"
			return environments.ScopeSelections{s}
		}, "not an exact kind"},
		{"wildcard action", slotOperation(false), func(_ *runnablev0.Operation, s *runnablev0.ScopeSelection) environments.ScopeSelections {
			s.Invoke[0].Actions = []string{"*"}
			return environments.ScopeSelections{s}
		}, "not an exact action"},
		{"fixed kind collision", slotOperation(true), func(_ *runnablev0.Operation, s *runnablev0.ScopeSelection) environments.ScopeSelections {
			s.Invoke[0].ResourceKind = "documents"
			return environments.ScopeSelections{s}
		}, "fixed invoke scope already binds"},
		{"two slots collide", slotOperation(false), func(o *runnablev0.Operation, s *runnablev0.ScopeSelection) environments.ScopeSelections {
			o.RequiredScopeSlots = append(o.RequiredScopeSlots, &runnablev0.ScopeSlot{Name: "other"})
			second := proto.CloneOf(s)
			second.Slot = "other"
			return environments.ScopeSelections{s, second}
		}, `both select kind "datasource.sources"`},
		{"undeclared slot", slotOperation(false), func(_ *runnablev0.Operation, s *runnablev0.ScopeSelection) environments.ScopeSelections {
			s.Slot = "typo"
			return environments.ScopeSelections{s}
		}, "does not declare"},
		{"duplicate slot", slotOperation(false), func(_ *runnablev0.Operation, s *runnablev0.ScopeSelection) environments.ScopeSelections {
			return environments.ScopeSelections{s, proto.CloneOf(s)}
		}, "selected twice"},
		{"lookup required", slotOperation(false), func(_ *runnablev0.Operation, s *runnablev0.ScopeSelection) environments.ScopeSelections {
			s.Lookup = nil
			return environments.ScopeSelections{s}
		}, "no lookup scope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selections := tt.change(tt.operation, sourceSelection())
			root, moduleDir := saveConnectRunnableFixture(t, context.Background(), descriptorSet(t, ingestionFile(tt.operation)), "0.1.0")
			t.Chdir(root)
			resetRunnablesFlags(t)
			resetRunnableBindingsFlags(t)
			require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
			key := RunnableBindingKey("documents", readIndex(t, moduleDir).Operations[0].Name)
			writeSelectionEnvironment(t, root, map[string]environments.ScopeSelections{key: selections})
			err := RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil)
			require.ErrorContains(t, err, tt.want)
			require.ErrorContains(t, err, "runnable-scope-selections["+key+"]")
			require.ErrorContains(t, err, "workspace.codefly.yaml")
			require.NoFileExists(t, filepath.Join(root, "configurations", "selected", RunnableBindingsGroup+".env"))
		})
	}
}

func TestFixedOnlyWithoutSelectionAndUnknownBindingKey(t *testing.T) {
	root, _ := saveConnectRunnableFixture(t, context.Background(), descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0")
	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	writeSelectionEnvironment(t, root, nil)
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	target := filepath.Join(root, "configurations", "selected", RunnableBindingsGroup+".env")
	before := readPreparedValues(t, target)
	for _, value := range before {
		verifyResolvedBinding(t, value, nil)
	}
	writeSelectionEnvironment(t, root, map[string]environments.ScopeSelections{"TYPO": {sourceSelection()}})
	require.ErrorContains(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil), "runnable-scope-selections[TYPO] names no derived operation")
	require.Equal(t, before, readPreparedValues(t, target))
}

func TestRunnableBindingsEnvSelectsAuthorityIndependentlyOfOutputProfile(t *testing.T) {
	root, moduleDir := saveConnectRunnableFixture(t, context.Background(), descriptorSet(t, ingestionFile(slotOperation(false))), "0.1.0")
	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	key := RunnableBindingKey("documents", readIndex(t, moduleDir).Operations[0].Name)
	writeSelectionEnvironment(t, root, map[string]environments.ScopeSelections{key: {sourceSelection()}})
	file := filepath.Join(root, "workspace.codefly.yaml")
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	var workspace map[string]any
	require.NoError(t, yaml.Unmarshal(data, &workspace))
	other := sourceSelection()
	other.Invoke[0].ResourceIds, other.Lookup[0].ResourceIds = []string{"source-b"}, []string{"source-b"}
	workspace["environments"] = append(workspace["environments"].([]any), &environments.Environment{
		Name: "local-fixture", ConfigurationProfile: "selected",
		RunnableScopeSelections: map[string]environments.ScopeSelections{key: {other}},
	})
	data, err = yaml.Marshal(workspace)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, data, 0o600))
	require.NoError(t, RunnableBindingsCmd.ParseFlags([]string{"--env", "local-fixture"}))
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	target := filepath.Join(root, "configurations", "selected", RunnableBindingsGroup+".env")
	prepared := verifyResolvedBinding(t, readPreparedValues(t, target)[key], slotOperation(false).Tool)
	require.Equal(t, []string{"source-b"}, prepared.Policy.InvokeScopes[0].ResourceIds)
	require.NoError(t, RunnableBindingsCmd.ParseFlags([]string{"--env", "local", "--check"}))
	require.ErrorContains(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil), "out of date")
}
