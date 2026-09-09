package orchestration

import (
	"testing"

	"github.com/codefly-dev/core/resources"
)

// --set supplies a bare service name, which two composed modules can each
// answer to. A caller that must reach exactly one service keys by the
// module-qualified unique, and that entry must not be diluted by a same-named
// service elsewhere in the graph — nor leak into it.
func TestFlowOverridesForPrefersTheModuleQualifiedKey(t *testing.T) {
	entry := serviceIn("wiki", "backend")
	sameNameElsewhere := serviceIn("documents", "backend")

	flow := &Flow{overrides: map[string]map[string]string{
		"wiki/backend": {"CODEFLY__API_CONSUMES": "[{\"id\":\"documents\"}]"},
	}}

	if got := flow.overridesFor(entry); got["CODEFLY__API_CONSUMES"] == "" {
		t.Fatalf("solution entry did not receive its override: %v", got)
	}
	if got := flow.overridesFor(sameNameElsewhere); len(got) != 0 {
		t.Fatalf("override leaked into documents/backend: %v", got)
	}
}

// The bare-name form is the documented --set contract and must keep reaching
// every service of that name, including when a module-qualified entry exists
// for a different service.
func TestFlowOverridesForFallsBackToTheBareName(t *testing.T) {
	flow := &Flow{overrides: map[string]map[string]string{
		"warden":       {"CODEFLY__FIXTURE": "dogfood"},
		"wiki/backend": {"CODEFLY__API_CONSUMES": "[]"},
	}}

	if got := flow.overridesFor(serviceIn("infra", "warden")); got["CODEFLY__FIXTURE"] != "dogfood" {
		t.Fatalf("--set by bare name stopped reaching its service: %v", got)
	}
	if got := flow.overridesFor(serviceIn("wiki", "backend")); got["CODEFLY__FIXTURE"] != "" {
		t.Fatalf("unrelated bare-name override bled into the qualified match: %v", got)
	}
}

func TestFlowOverridesForEmpty(t *testing.T) {
	flow := &Flow{}
	if got := flow.overridesFor(serviceIn("wiki", "backend")); got != nil {
		t.Fatalf("expected nil overrides, got %v", got)
	}
}

func serviceIn(module string, name string) *resources.Service {
	service := &resources.Service{Name: name}
	service.WithModule(module)
	return service
}

// When both forms name the same service they must layer, not compete: the run
// path derives module-qualified overrides of its own (the federation secrets a
// solution's registrar needs), so returning only the qualified map made
// `--set accounts:LOG_LEVEL=debug` vanish from a service the operator never
// asked the CLI to touch. The qualified entry still wins key by key.
func TestFlowOverridesForLayersBareNameUnderTheQualifiedKey(t *testing.T) {
	flow := &Flow{overrides: map[string]map[string]string{
		"accounts":      {"LOG_LEVEL": "debug", "MODULE_REGISTRATION_SECRETS": "operator-pinned"},
		"host/accounts": {"MODULE_REGISTRATION_SECRETS": "documents:deadbeef"},
	}}

	got := flow.overridesFor(serviceIn("host", "accounts"))
	if got["LOG_LEVEL"] != "debug" {
		t.Errorf("--set by bare name was dropped by a derived qualified override: %v", got)
	}
	if got["MODULE_REGISTRATION_SECRETS"] != "documents:deadbeef" {
		t.Errorf("qualified entry must win key by key, got %v", got)
	}
}

// Layering must not write through into the stored maps: a second service of the
// same name resolves from the same entries and would otherwise inherit whatever
// the first merge left behind.
func TestFlowOverridesForDoesNotMutateItsEntries(t *testing.T) {
	flow := &Flow{overrides: map[string]map[string]string{
		"accounts":      {"LOG_LEVEL": "debug"},
		"host/accounts": {"MODULE_REGISTRATION_SECRETS": "documents:deadbeef"},
	}}

	flow.overridesFor(serviceIn("host", "accounts"))

	if _, leaked := flow.overrides["accounts"]["MODULE_REGISTRATION_SECRETS"]; leaked {
		t.Errorf("merge wrote through into the bare-name entry: %v", flow.overrides["accounts"])
	}
	if _, leaked := flow.overrides["host/accounts"]["LOG_LEVEL"]; leaked {
		t.Errorf("merge wrote through into the qualified entry: %v", flow.overrides["host/accounts"])
	}
	if got := flow.overridesFor(serviceIn("solution", "accounts")); got["MODULE_REGISTRATION_SECRETS"] != "" {
		t.Errorf("a same-named service in another module inherited the qualified value: %v", got)
	}
}
