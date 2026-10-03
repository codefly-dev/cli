package deploysecrets

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

// rootGroupCredentialKey is the store key a GitOps render produces for a
// credential-named value in a composition-root workspace configuration group.
//
// It is derived, not written down: the render promotes such a value to a
// `secretKeyRef` whose key is core's environment encoding of the group and the
// key (pkg/orchestration promotableConfiguration →
// resources.ConfigurationAsEnvironmentVariables). Deriving it the same way is
// what makes this test about the key the render actually emits rather than about
// a string that was true once. The render side of the same claim is pinned by
// pkg/orchestration's TestACompositionRootGroupRendersItsCredentialsByReference,
// which asserts the reference it produces equals this encoding. The two halves
// meet on core's encoding rather than on one shared call because orchestration's
// promotion path (promotableDeploymentConfigurations) is unexported — not
// because of the import graph: this package may import pkg/orchestration, and
// only the reverse would cycle through pkg/gitops.
func rootGroupCredentialKey(t *testing.T, group, key string) string {
	t.Helper()
	variables, err := resources.ConfigurationAsEnvironmentVariables(&basev0.Configuration{
		Origin: resources.ConfigurationWorkspace,
		Infos: []*basev0.ConfigurationInformation{{
			Name:                group,
			ConfigurationValues: []*basev0.ConfigurationValue{{Key: key, Secret: true}},
		}},
	}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(variables) != 1 {
		t.Fatalf("ConfigurationAsEnvironmentVariables(%s/%s) = %d variables, want 1", group, key, len(variables))
	}
	return variables[0].Key
}

// renderedRootCredential is one rendered ExternalSecret reading one key, filed
// under a property of its own name — the shape an environment that declares no
// remote-key mapping emits.
func renderedRootCredential(storeKey string) gitops.RenderedEnvironment {
	return gitops.RenderedEnvironment{
		Modules: []string{"payments"},
		Secrets: []gitops.RenderedServiceSecret{{
			Store:     environments.EnvironmentSecretStoreReference{Name: "cell-secrets", Kind: "ClusterSecretStore"},
			RemoteKey: "payments-api",
			Services:  []string{"payments/api"},
			Properties: []gitops.RenderedSecretProperty{{
				Property: storeKey,
				Keys:     []string{storeKey},
				Readers:  []gitops.RenderedSecretReader{{Service: "payments/api", Key: storeKey}},
			}},
		}},
	}
}

// What `codefly deploy secrets` does with a credential a composition-root group
// newly brought into the render, against a store that does not hold it: it
// discovers the key and reports that nothing can supply the value.
//
// Re-running the verb seeds a property only from a source the planner can
// reach — a value the store already holds, a federation derivation, another
// remote key holding the same configuration value, or a
// `service-secrets.generate` declaration. An arbitrary root-group credential is
// none of those: the render discarded its plaintext precisely so a committed
// manifest never carries it, and the planner never sees the configuration value.
// fallbackSource therefore returns ActionRequire and the operator supplies it.
//
// The key follows the render. The value does not follow the key. That is the
// correction to the render documentation, and it matters because the failure is
// quiet in the wrong place: the plan says `require`, so an operator who reads it
// acts, while one who promotes on the strength of "the key follows the next time
// it runs" ships a workload whose ExternalSecret has nothing to materialize.
func TestARootGroupCredentialWithNoSourceIsRequiredNotSeeded(t *testing.T) {
	ctx := context.Background()
	storeKey := rootGroupCredentialKey(t, "work-context", "authority-token")
	store := newFakeStore(map[string]map[string]string{})

	plan, err := Build(ctx, &Inputs{
		Rendered: renderedRootCredential(storeKey), Store: store,
		ReadPayloads: true, Services: []string{"payments/api"},
	})
	if err != nil {
		t.Fatal(err)
	}

	property := propertyPlan(t, plan, "payments-api", storeKey)
	if property.Action != ActionRequire {
		t.Errorf("a root group credential with no holder, no derivation and no declared generator = %s, want %s",
			property.Action, ActionRequire)
	}
	if !strings.Contains(property.Source, "no key holds it") {
		t.Errorf("Source = %q, want it to say why no value can be supplied", property.Source)
	}
	want := []string{"payments-api#" + storeKey + " (" + storeKey + ")"}
	if got := plan.Required(); !slices.Equal(got, want) {
		t.Errorf("Required() = %v, want %v: the operator must be told the key and where it is filed", got, want)
	}
	if got := plan.Changes(); len(got) != 0 {
		t.Errorf("Changes() = %v, want nothing: no value can be produced", got)
	}
	if _, err := plan.Apply(ctx, store, true); err != nil {
		t.Fatal(err)
	}
	if document, held := store.documents["payments-api"]; held && document[storeKey] != "" {
		t.Error("an apply invented a value for a property nothing can produce")
	}
}

// The remedy the refusal must name: the environment declaring a generator for
// that key. Asserted beside it so the documented dead end is documented with a
// way out, and so a later change that stops honouring the declaration for a
// workspace-scoped root group fails here.
func TestARootGroupCredentialTheEnvironmentDeclaresIsGenerated(t *testing.T) {
	ctx := context.Background()
	storeKey := rootGroupCredentialKey(t, "work-context", "authority-token")
	store := newFakeStore(map[string]map[string]string{})

	plan, err := Build(ctx, &Inputs{
		Rendered: renderedRootCredential(storeKey), Store: store,
		ReadPayloads: true, Services: []string{"payments/api"},
		Generators: []environments.EnvironmentSecretGenerator{{
			Scope:         environments.SecretGeneratorScopeWorkspace,
			Configuration: "work-context",
			Keys:          []string{"AUTHORITY_TOKEN"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := propertyPlan(t, plan, "payments-api", storeKey); got.Action != ActionGenerate {
		t.Errorf("a declared generator for the key = %s, want %s", got.Action, ActionGenerate)
	}
	if got := plan.Required(); len(got) != 0 {
		t.Errorf("Required() = %v, want nothing: the declaration is what supplies the value", got)
	}
	if _, err := plan.Apply(ctx, store, true); err != nil {
		t.Fatal(err)
	}
	if store.documents["payments-api"][storeKey] == "" {
		t.Error("the declared generator wrote no value")
	}
}
