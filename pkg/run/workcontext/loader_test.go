package workcontext

import (
	"context"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

type stubLoader struct {
	configurations []*basev0.Configuration
	rootNames      []string
}

func (s *stubLoader) Identity() string                                     { return "stub" }
func (s *stubLoader) Load(context.Context, *resources.Environment) error   { return nil }
func (s *stubLoader) Configurations() []*basev0.Configuration              { return s.configurations }
func (s *stubLoader) DNS() []*basev0.DNS                                   { return nil }
func (s *stubLoader) CompositionRootWorkspaceConfigurationNames() []string { return s.rootNames }

func group(name string) *basev0.Configuration {
	return &basev0.Configuration{Infos: []*basev0.ConfigurationInformation{{Name: name}}}
}

func groupsIn(configs []*basev0.Configuration) map[string]bool {
	out := map[string]bool{}
	for _, configuration := range configs {
		for _, info := range configuration.GetInfos() {
			out[info.GetName()] = true
		}
	}
	return out
}

func load(t *testing.T, inner *stubLoader, authority *Authority, declared []string, env *resources.Environment) map[string]bool {
	t.Helper()
	loader := WithKeySet(inner, authority, declared)
	if err := loader.Load(t.Context(), env); err != nil {
		t.Fatal(err)
	}
	return groupsIn(loader.Configurations())
}

func authority(t *testing.T) *Authority {
	t.Helper()
	made, err := Start("run-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = made.Close() })
	return made
}

// A deployed composition that declares a group and supplies nothing has a
// misconfiguration, and the manager refusing to start is how it is found.
// Covering it here would turn a loud boot failure into a service running
// without the capability it said it needed.
func TestADeployedRunIsNotGivenGroupsItsCompositionDidNotSupply(t *testing.T) {
	groups := load(t, &stubLoader{}, authority(t), []string{"platform", "internal-auth"},
		&resources.Environment{Name: "production"})
	for _, group := range []string{"platform", "internal-auth"} {
		if groups[group] {
			t.Errorf("%q was routed in empty outside a local environment, hiding a composition that supplies it nowhere", group)
		}
	}
}

func TestALocalRunIsGivenTheDeclaredGroupsNothingSupplied(t *testing.T) {
	groups := load(t, &stubLoader{}, authority(t), []string{"platform", "internal-auth"},
		resources.LocalEnvironment())
	for _, group := range []string{"platform", "internal-auth"} {
		if !groups[group] {
			t.Errorf("%q was not routed in, so the service fails at the manager before it can decide what absence means", group)
		}
	}
}

// These two were conjoined: an early return skipped the declared groups
// whenever work-context happened to be authored, so a composition that
// authored one group lost the rest.
func TestAnAuthoredKeySetDoesNotSuppressTheOtherDeclaredGroups(t *testing.T) {
	inner := &stubLoader{configurations: []*basev0.Configuration{group("work-context")}}
	groups := load(t, inner, authority(t), []string{"work-context", "platform"}, resources.LocalEnvironment())
	if !groups["platform"] {
		t.Error("platform was skipped because work-context was authored; the two are unrelated")
	}
}

// A composition that says where its key set is served keeps saying it.
func TestAnAuthoredKeySetIsNotReplacedByTheRuns(t *testing.T) {
	inner := &stubLoader{configurations: []*basev0.Configuration{group("work-context")}}
	loader := WithKeySet(inner, authority(t), nil)
	if err := loader.Load(t.Context(), resources.LocalEnvironment()); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, configuration := range loader.Configurations() {
		for _, info := range configuration.GetInfos() {
			if info.GetName() == "work-context" {
				count++
			}
		}
	}
	if count != 1 {
		t.Fatalf("work-context appears %d times; the run added one beside the composition's own", count)
	}
}

// Swallowing this would leave the Manager unable to tell which groups come
// from the composition root, which it injects into every service in the run.
func TestTheCompositionRootCapabilityIsForwarded(t *testing.T) {
	inner := &stubLoader{rootNames: []string{"platform"}}
	loader := WithKeySet(inner, authority(t), nil)
	forwarder, ok := loader.(interface {
		CompositionRootWorkspaceConfigurationNames() []string
	})
	if !ok {
		t.Fatal("the decorator does not carry the composition-root capability at all")
	}
	if names := forwarder.CompositionRootWorkspaceConfigurationNames(); len(names) != 1 || names[0] != "platform" {
		t.Errorf("the capability is not forwarded: %v", names)
	}
}
