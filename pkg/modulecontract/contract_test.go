package modulecontract

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", FileName))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func values() MapValues {
	return MapValues{
		Public: map[string]map[string]string{
			"assistant": {
				"MODEL_PROFILE":          "model-gateway",
				"MODEL_RESOURCE_KIND":    "modelservice.profiles",
				"model-binding":          "model",
				"DOCUMENTS_ENDPOINT":     "documents",
				"EVIDENCE_RESOURCE_KIND": "documents.passages",
				"ANNOTATIONS_PREFIX":     "annotations",
			},
		},
	}
}

// TestParsesTheAgreedShape pins the file shape the modules of the lifecycle
// publish against: the path, the schema string and the field split.
func TestParsesTheAgreedShape(t *testing.T) {
	contract, err := Parse(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if contract.Principal != "assistant" || len(contract.Bindings) != 3 || len(contract.Destinations) != 2 {
		t.Fatalf("contract %+v", contract)
	}
	if contract.Queues == nil || len(contract.Queues) != 0 {
		t.Fatalf("an empty queue list must survive as declared-empty, got %#v", contract.Queues)
	}
	if contract.Bindings[0].Audience.Group() != "assistant" || contract.Bindings[0].Audience.Key() != "model-profile" {
		t.Fatalf("slot %+v", contract.Bindings[0].Audience)
	}
	loaded, err := Load("testdata")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Principal != contract.Principal {
		t.Fatal("Load read a different contract than Parse")
	}
	if _, err = Load(t.TempDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a module without a contract must report os.ErrNotExist, got %v", err)
	}
}

func TestResolvesEverySlotFromPublicConfiguration(t *testing.T) {
	contract, err := Parse(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := contract.Resolve(values())
	if err != nil {
		t.Fatal(err)
	}
	model := resolved.Bindings[0]
	if model.Audience != "model-gateway" || model.ResourceKind != "modelservice.profiles" || model.BindingKey != "model" {
		t.Fatalf("model binding %+v", model)
	}
	if model.Revision != 1 || resolved.Bindings[1].Revision != 2 {
		t.Fatalf("revisions: %d and %d; an omitted revision is 1", model.Revision, resolved.Bindings[1].Revision)
	}
	if got := strings.Join(model.Scopes["invoke"], ","); got != "modelservice.profiles:invoke,modelservice.profiles:read" {
		t.Fatalf("invoke scopes %q", got)
	}
	if got := strings.Join(model.Scopes["lookup"], ","); got != "modelservice.profiles:read" {
		t.Fatalf("lookup scopes %q", got)
	}
	// A binding without a resource kind carries bare actions.
	if got := strings.Join(resolved.Bindings[2].Scopes["headless"], ","); got != "read,write" {
		t.Fatalf("headless scopes %q", got)
	}
}

func TestRefusesASecretOrUnresolvedSlotNamingEveryOne(t *testing.T) {
	contract, err := Parse(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	partial := values()
	delete(partial.Public["assistant"], "MODEL_PROFILE")
	delete(partial.Public["assistant"], "EVIDENCE_RESOURCE_KIND")
	_, err = contract.Resolve(partial)
	if !errors.Is(err, ErrUnresolvedSlot) {
		t.Fatalf("unresolved slots were not refused: %v", err)
	}
	for _, want := range []string{"binding model audience ← assistant/model-profile", "binding evidence resource_kind ← assistant/evidence-resource-kind"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}

	leaked := values()
	leaked.Secrets = map[string]map[string]string{"assistant": {"ANNOTATIONS_PREFIX": "annotations"}}
	_, err = contract.Resolve(leaked)
	if !errors.Is(err, ErrSecretSlot) || !strings.Contains(err.Error(), "binding annotations audience") {
		t.Fatalf("a secret-classified slot was not refused by name: %v", err)
	}
}

func TestRefusesWhatAModuleMayNotAssert(t *testing.T) {
	base := string(fixture(t))
	for name, table := range map[string]struct {
		mutate func(string) string
		want   error
		text   string
	}{
		"another schema": {
			mutate: func(s string) string { return strings.Replace(s, SchemaV1, "codefly/module-contract/v2", 1) },
			want:   ErrSchema,
		},
		"tenancy is the envelope's": {
			mutate: func(s string) string { return s + "tenancy: dedicated\n" },
			want:   ErrInvalid, text: "tenancy",
		},
		"a literal audience": {
			mutate: func(s string) string {
				return strings.Replace(s, "audience: {from: assistant/model-profile}", "audience: model-gateway", 1)
			},
			want: ErrInvalid, text: "a slot is {from: <group>/<key>}",
		},
		"a slot with a default": {
			mutate: func(s string) string {
				return strings.Replace(s, "audience: {from: assistant/model-profile}", "audience: {from: assistant/model-profile, default: x}", 1)
			},
			want: ErrInvalid, text: "unknown slot field",
		},
		"an operation without a ceiling": {
			mutate: func(s string) string {
				return strings.Replace(s, "operations: [invoke]\n", "operations: [invoke, headless]\n", 1)
			},
			want: ErrInvalid, text: "no scope ceiling",
		},
		"a ceiling for an undeclared operation": {
			mutate: func(s string) string {
				return strings.Replace(s, "          headless: [read, write]\n", "          headless: [read, write]\n          invoke: [read]\n", 1)
			},
			want: ErrInvalid, text: "does not declare",
		},
		"queues omitted": {
			mutate: func(s string) string { return strings.Replace(s, "queues: []\n", "", 1) },
			want:   ErrInvalid, text: "queues must be declared",
		},
		"an unknown operation": {
			mutate: func(s string) string { return strings.Replace(s, "operations: [headless]", "operations: [execute]", 1) },
			want:   ErrInvalid, text: "not one of",
		},
		"an unknown destination kind": {
			mutate: func(s string) string { return strings.Replace(s, "kind: platform-internal", "kind: cluster", 1) },
			want:   ErrInvalid, text: "kind",
		},
		"a duplicate binding": {
			mutate: func(s string) string { return strings.Replace(s, "id: evidence", "id: model", 1) },
			want:   ErrInvalid, text: "declared twice",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(table.mutate(base)))
			if !errors.Is(err, table.want) {
				t.Fatalf("got %v, want %v", err, table.want)
			}
			if table.text != "" && !strings.Contains(err.Error(), table.text) {
				t.Fatalf("refusal %q does not say %q", err, table.text)
			}
		})
	}
}
