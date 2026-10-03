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
				"MODEL_AUDIENCE":         "model-gateway",
				"MODEL_RESOURCE_KIND":    "modelservice.profiles",
				"model-binding":          "model",
				"EVIDENCE_AUDIENCE":      "documents",
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
	if contract.Bindings[0].Audience.Group() != "assistant" || contract.Bindings[0].Audience.Key() != "model-audience" {
		t.Fatalf("slot %+v", contract.Bindings[0].Audience)
	}
	// Both ceiling spellings survive parsing as what they are.
	if got := contract.Bindings[0].ScopeCeiling["invoke"]; len(got.Actions) != 2 || len(got.Scopes) != 0 {
		t.Fatalf("bare ceiling %+v", got)
	}
	if got := contract.Bindings[2].ScopeCeiling["headless"]; len(got.Actions) != 0 || len(got.Scopes) != 2 || got.Scopes[1].ResourceKind != "annotations.annotations" {
		t.Fatalf("explicit ceiling %+v", got)
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
	// An explicit ceiling resolves to the kinds it names, so a binding whose
	// acts span two kinds yields one scope per (kind, action), sorted.
	if got := strings.Join(resolved.Bindings[2].Scopes["headless"], ","); got != "annotations.annotations:redact,annotations.vocabularies:write" {
		t.Fatalf("headless scopes %q", got)
	}
}

func TestRefusesASecretOrUnresolvedSlotNamingEveryOne(t *testing.T) {
	contract, err := Parse(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	partial := values()
	delete(partial.Public["assistant"], "MODEL_AUDIENCE")
	delete(partial.Public["assistant"], "EVIDENCE_RESOURCE_KIND")
	_, err = contract.Resolve(partial)
	if !errors.Is(err, ErrUnresolvedSlot) {
		t.Fatalf("unresolved slots were not refused: %v", err)
	}
	for _, want := range []string{"binding model audience ← assistant/model-audience", "binding evidence resource_kind ← assistant/evidence-resource-kind"} {
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

// TestASlotKeyCarriesItsMeaning pins the naming convention: a declared,
// supplied key that is not an audience resolves cleanly into a binding
// addressed to the wrong thing, so the reader refuses the key's name rather
// than trusting that a value means what the slot says.
func TestASlotKeyCarriesItsMeaning(t *testing.T) {
	base := string(fixture(t))
	for name, table := range map[string]struct {
		mutate func(string) string
		want   string
	}{
		"an audience from a profile key": {
			mutate: func(s string) string {
				return strings.Replace(s, "assistant/model-audience", "assistant/model-profile", 1)
			},
			want: "binding model audience slot \"assistant/model-profile\" names a key that is not *-audience or *-prefix",
		},
		"a resource kind from an endpoint key": {
			mutate: func(s string) string {
				return strings.Replace(s, "assistant/evidence-resource-kind", "assistant/documents-endpoint", 1)
			},
			want: "binding evidence resource_kind slot \"assistant/documents-endpoint\" names a key that is not *-resource-kind",
		},
		"a binding key from a bare name": {
			mutate: func(s string) string { return strings.Replace(s, "assistant/model-binding", "assistant/model", 1) },
			want:   "binding model binding_key slot \"assistant/model\" names a key that is not *-binding",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(table.mutate(base)))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), table.want) {
				t.Fatalf("got %v, want %q", err, table.want)
			}
		})
	}
	// Either spelling core accepts passes the convention.
	upper := strings.Replace(base, "assistant/model-audience", "assistant/MODEL_AUDIENCE", 1)
	if _, err := Parse([]byte(upper)); err != nil {
		t.Fatalf("the upper-case spelling of a conventional key was refused: %v", err)
	}
}

// TestACeilingIsWrittenInOneSpelling pins the two ceiling spellings and what
// each needs: bare actions need the binding's resource kind to qualify them,
// because a scope without a kind is one nothing can mint; explicit scopes name
// their own kind, exactly once each, with at least one action; and one list
// never mixes the two.
func TestACeilingIsWrittenInOneSpelling(t *testing.T) {
	base := string(fixture(t))
	for name, table := range map[string]struct {
		mutate func(string) string
		want   string
	}{
		"bare actions with no resource kind to qualify them": {
			mutate: func(s string) string {
				return strings.Replace(s, "          headless:\n              - resource_kind: annotations.vocabularies\n                actions: [write]\n              - resource_kind: annotations.annotations\n                actions: [redact]\n",
					"          headless: [write, redact]\n", 1)
			},
			want: "binding annotations headless ceiling lists bare actions but the binding declares no resource_kind slot",
		},
		"a mixed list": {
			mutate: func(s string) string {
				return strings.Replace(s, "              - resource_kind: annotations.annotations\n                actions: [redact]\n", "              - redact\n", 1)
			},
			want: "mixes {resource_kind, actions} entries with bare actions",
		},
		"a kind named twice": {
			mutate: func(s string) string {
				return strings.Replace(s, "resource_kind: annotations.annotations", "resource_kind: annotations.vocabularies", 1)
			},
			want: "names resource kind \"annotations.vocabularies\" twice",
		},
		"a kind with no action": {
			mutate: func(s string) string { return strings.Replace(s, "actions: [redact]", "actions: []", 1) },
			want:   "resource kind \"annotations.annotations\" permits no action",
		},
		"an entry carrying a field of its own": {
			mutate: func(s string) string {
				return strings.Replace(s, "                actions: [redact]\n", "                actions: [redact]\n                resource_ids: [one]\n", 1)
			},
			want: "unknown scope ceiling field \"resource_ids\"",
		},
		"a ceiling that is not a list": {
			mutate: func(s string) string {
				return strings.Replace(s, "          lookup: [read]\n", "          lookup: read\n", 1)
			},
			want: "a scope ceiling is a list of actions or of {resource_kind, actions} entries",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(table.mutate(base)))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), table.want) {
				t.Fatalf("got %v, want %q", err, table.want)
			}
		})
	}
}

// TestReadsTheShapeAModulePublishes is the shape a module of the lifecycle
// actually published (its annotations binding spans two kinds and its slots
// follow the key convention), reduced to neutral names: the reader exists for
// this file, so it must read it.
func TestReadsTheShapeAModulePublishes(t *testing.T) {
	const published = `schema: codefly/module-contract/v1
principal: helper
namespaces:
    - helper
queues: []
scope_ceilings:
    - resource_kind: helper.tasks
      actions: [execute, read]
    - resource_kind: helper.runs
      actions: [read, start, cancel]
bindings:
    - id: model
      operations: [invoke, lookup]
      audience: {from: helper/model-audience}
      resource_kind: {from: helper/model-resource-kind}
      binding_key: {from: helper/model-binding}
      scope_ceiling:
          invoke: [invoke, read]
          lookup: [read]
    - id: annotations
      operations: [headless]
      audience: {from: helper/annotations-prefix}
      binding_key: {from: helper/annotations-binding}
      scope_ceiling:
          headless:
              - resource_kind: annotations.vocabularies
                actions: [write]
              - resource_kind: annotations.annotations
                actions: [redact]
destinations:
    - id: chat-http
      service: chat
      endpoint: http
      kind: module
`
	contract, err := Parse([]byte(published))
	if err != nil {
		t.Fatalf("the published shape is refused: %v", err)
	}
	resolved, err := contract.Resolve(MapValues{Public: map[string]map[string]string{"helper": {
		"MODEL_AUDIENCE": "model-gateway", "MODEL_RESOURCE_KIND": "modelservice.profiles", "MODEL_BINDING": "model",
		"ANNOTATIONS_PREFIX": "annotations", "ANNOTATIONS_BINDING": "annotations",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(resolved.Bindings[1].Scopes["headless"], ","); got != "annotations.annotations:redact,annotations.vocabularies:write" {
		t.Fatalf("headless scopes %q", got)
	}
	if resolved.Bindings[1].BindingKey != "annotations" || resolved.Bindings[1].ResourceKind != "" {
		t.Fatalf("annotations binding %+v", resolved.Bindings[1])
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
				return strings.Replace(s, "audience: {from: assistant/model-audience}", "audience: model-gateway", 1)
			},
			want: ErrInvalid, text: "a slot is {from: <group>/<key>}",
		},
		"a slot with a default": {
			mutate: func(s string) string {
				return strings.Replace(s, "audience: {from: assistant/model-audience}", "audience: {from: assistant/model-audience, default: x}", 1)
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
				return strings.Replace(s, "          lookup: [read]\n", "          lookup: [read]\n          headless: [read]\n", 1)
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
