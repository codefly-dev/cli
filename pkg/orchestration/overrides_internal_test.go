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
