package deploysecrets

import (
	"context"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
)

// namedStore reports a backend identity rather than the store reference's name,
// the way ResolveStore does ("gcpsm project <projectID>"): two store references
// can resolve to one backend.
type namedStore struct {
	backend  string
	document map[string]string
	reads    []string
	writes   map[string]map[string]string
}

func (store *namedStore) Name() string { return store.backend }

func (store *namedStore) Describe(context.Context, string) (Description, error) {
	return Description{Exists: true, HasVersion: true}, nil
}

func (store *namedStore) Read(_ context.Context, key string) (map[string]string, error) {
	store.reads = append(store.reads, key)
	out := map[string]string{}
	for property, value := range store.document {
		out[property] = value
	}
	return out, nil
}

func (store *namedStore) Write(_ context.Context, key string, document map[string]string, _ bool) error {
	if store.writes == nil {
		store.writes = map[string]map[string]string{}
	}
	store.writes[key] = document
	return nil
}

func TestStoreGroupsSeparateANamespacedStorePerNamespace(t *testing.T) {
	rendered := gitops.RenderedEnvironment{}
	for _, kind := range []string{"SecretStore", "ClusterSecretStore"} {
		for _, namespace := range []string{"a", "b"} {
			rendered.Secrets = append(rendered.Secrets, gitops.RenderedServiceSecret{
				Store:     environments.EnvironmentSecretStoreReference{Name: "store", Kind: kind},
				Namespace: namespace, RemoteKey: "key",
			})
		}
	}
	groups := StoreGroups(rendered)
	// A ClusterSecretStore is shared across namespaces, so its two keys are one
	// group; the namespaced SecretStore's two namespaces are two objects.
	if len(groups) != 3 || len(groups[0].Secrets) != 2 ||
		groups[1].Secrets[0].Namespace != "a" || groups[2].Secrets[0].Namespace != "b" {
		t.Fatalf("grouped wrong store scope: %+v", groups)
	}
}

// Two store references that resolve to ONE backend are one plan. Store.Name() is
// the backend identity ("gcpsm project P"), and a ClusterSecretStore and a
// namespaced SecretStore can both address that project — planning them apart is
// what minted one configuration value twice.
func TestBuildAllCoalescesStoreReferencesResolvingToOneBackend(t *testing.T) {
	store := &namedStore{backend: "gcpsm project shared", document: map[string]string{}}
	rendered := gitops.RenderedEnvironment{Secrets: []gitops.RenderedServiceSecret{
		{Store: environments.EnvironmentSecretStoreReference{Name: "cluster-wide", Kind: "ClusterSecretStore"},
			RemoteKey: "shared-key", Services: []string{"app/api"},
			Properties: []gitops.RenderedSecretProperty{{Property: "token", Keys: []string{"APP_TOKEN"}}}},
		{Store: environments.EnvironmentSecretStoreReference{Name: "in-namespace", Kind: "SecretStore"},
			Namespace: "web", RemoteKey: "shared-key", Services: []string{"web/console"},
			Properties: []gitops.RenderedSecretProperty{{Property: "other", Keys: []string{"WEB_TOKEN"}}}},
	}}
	resolutions := 0
	plans, err := BuildAll(context.Background(), &Inputs{Rendered: rendered, ReadPayloads: true},
		func(gitops.RenderedEnvironment) (Store, error) {
			resolutions++
			return store, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if resolutions != 2 {
		t.Fatalf("resolved %d store references, want one per reference", resolutions)
	}
	if len(plans) != 1 {
		t.Fatalf("planned %d backends for one backend", len(plans))
	}
	// One remote key, named by both references, is described and read once and
	// planned once — not twice with two writes racing over one document.
	if len(plans[0].Plan.Secrets) != 1 || plans[0].Plan.Secrets[0].RemoteKey != "shared-key" {
		t.Fatalf("remote key not merged: %+v", plans[0].Plan.Secrets)
	}
	if len(store.reads) != 1 {
		t.Fatalf("read the merged key %d times: %v", len(store.reads), store.reads)
	}
	properties := plans[0].Plan.Secrets[0].Properties
	if len(properties) != 2 || properties[0].Property != "other" || properties[1].Property != "token" {
		t.Fatalf("merged key lost a property: %+v", properties)
	}
}

// A configuration value filed under DIFFERENT property names in two backends is
// still one value: the alias index spans backends, so the divergence is seen.
func TestBuildAllComparesAcrossBackendsThroughPropertyAliases(t *testing.T) {
	const key = "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__AUTH__TOKEN"
	alpha := &namedStore{backend: "alpha", document: map[string]string{"token": "one"}}
	beta := &namedStore{backend: "beta", document: map[string]string{"internal_token": "two"}}
	rendered := gitops.RenderedEnvironment{Secrets: []gitops.RenderedServiceSecret{
		{Store: environments.EnvironmentSecretStoreReference{Name: "alpha", Kind: "ClusterSecretStore"},
			RemoteKey: "alpha-key", Services: []string{"app/api"},
			Properties: []gitops.RenderedSecretProperty{{Property: "token", Keys: []string{key}}}},
		{Store: environments.EnvironmentSecretStoreReference{Name: "beta", Kind: "ClusterSecretStore"},
			RemoteKey: "beta-key", Services: []string{"web/console"},
			Properties: []gitops.RenderedSecretProperty{{Property: "internal_token", Keys: []string{key}}}},
	}}
	_, err := BuildAll(context.Background(), &Inputs{Rendered: rendered, ReadPayloads: true},
		func(group gitops.RenderedEnvironment) (Store, error) {
			if group.Secrets[0].Store.Name == "alpha" {
				return alpha, nil
			}
			return beta, nil
		})
	if err == nil {
		t.Fatal("one configuration value filed under two property names in two backends was accepted")
	}
	if !strings.Contains(err.Error(), "one configuration value cannot be two") {
		t.Fatalf("unexpected refusal: %v", err)
	}
}
