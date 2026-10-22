package deploy

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/deploysecrets"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
)

func withSecretsFlags(t *testing.T, dryRun, metadataOnly bool, modules []string) {
	t.Helper()
	previousDryRun, previousMetadata, previousModules := secretsDryRun, secretsMetadataOnly, secretsModules
	t.Cleanup(func() {
		secretsDryRun, secretsMetadataOnly, secretsModules = previousDryRun, previousMetadata, previousModules
	})
	secretsDryRun, secretsMetadataOnly, secretsModules = dryRun, metadataOnly, modules
}

// --metadata-only never reads a stored value, so it cannot know what a write
// would overwrite. The refusal comes before the workspace is even loaded, so an
// operator is told immediately rather than after the store has been walked.
func TestSecretsRefusesMetadataOnlyWithoutDryRun(t *testing.T) {
	withSecretsFlags(t, false, true, nil)
	err := SecretsCmd.RunE(SecretsCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--dry-run") {
		t.Fatalf("RunE = %v, want a refusal naming --dry-run", err)
	}
}

// One plan writes one store. A render reading through two stores is split into
// one group per store, each carrying the render's module lists, so each store
// is planned and written on its own rather than the render being refused.
func TestRenderedByStoreSplitsOneGroupPerStore(t *testing.T) {
	cell := environments.EnvironmentSecretStoreReference{Name: "cell-secrets", Kind: "ClusterSecretStore"}
	identity := environments.EnvironmentSecretStoreReference{Name: "platform-identity", Kind: "ClusterSecretStore"}
	rendered := gitops.RenderedEnvironment{
		Modules: []string{"saas", "documents"},
		Secrets: []gitops.RenderedServiceSecret{
			{Store: cell, RemoteKey: "a"},
			{Store: identity, RemoteKey: "b"},
			{Store: cell, RemoteKey: "c"},
		},
	}
	groups := renderedByStore(rendered)
	if len(groups) != 2 {
		t.Fatalf("renderedByStore = %d groups, want 2", len(groups))
	}
	if groups[0].Secrets[0].Store != cell || len(groups[0].Secrets) != 2 || groups[1].Secrets[0].Store != identity || len(groups[1].Secrets) != 1 {
		t.Fatalf("renderedByStore groups = %+v, want cell-secrets (a, c) then platform-identity (b)", groups)
	}
	for _, group := range groups {
		if strings.Join(group.Modules, ",") != "saas,documents" {
			t.Fatalf("group modules = %v, want the render's", group.Modules)
		}
	}
	// A namespaced SecretStore is one store per namespace.
	namespaced := environments.EnvironmentSecretStoreReference{Name: "local", Kind: "SecretStore"}
	perNamespace := renderedByStore(gitops.RenderedEnvironment{Secrets: []gitops.RenderedServiceSecret{
		{Store: namespaced, Namespace: "saas", RemoteKey: "a"},
		{Store: namespaced, Namespace: "documents", RemoteKey: "b"},
	}})
	if len(perNamespace) != 2 {
		t.Fatalf("renderedByStore(namespaced) = %d groups, want 2", len(perNamespace))
	}
}

// A mixed group never reaches a store: resolveSecretStore names both stores
// rather than seeding whichever it saw first.
func TestResolveSecretStoreRefusesAMixedGroup(t *testing.T) {
	rendered := gitops.RenderedEnvironment{Secrets: []gitops.RenderedServiceSecret{
		{Store: environments.EnvironmentSecretStoreReference{Name: "cell-secrets", Kind: "ClusterSecretStore"}, RemoteKey: "a"},
		{Store: environments.EnvironmentSecretStoreReference{Name: "other-secrets", Kind: "ClusterSecretStore"}, RemoteKey: "b"},
	}}
	_, err := resolveSecretStore(context.Background(), &environments.Environment{Name: "staging"}, rendered)
	if err == nil || !strings.Contains(err.Error(), "cell-secrets") || !strings.Contains(err.Error(), "other-secrets") {
		t.Fatalf("resolveSecretStore = %v, want a refusal naming both stores", err)
	}
}

// The store is resolved from the environment's cluster, so an environment that
// names no cluster context cannot be seeded — and is told that, rather than
// failing inside kubectl.
func TestResolveSecretStoreRequiresAClusterContext(t *testing.T) {
	rendered := gitops.RenderedEnvironment{Secrets: []gitops.RenderedServiceSecret{
		{Store: environments.EnvironmentSecretStoreReference{Name: "cell-secrets", Kind: "ClusterSecretStore"}, RemoteKey: "a"},
	}}
	_, err := resolveSecretStore(context.Background(), &environments.Environment{Name: "staging"}, rendered)
	if err == nil || !strings.Contains(err.Error(), "cluster.context") {
		t.Fatalf("resolveSecretStore = %v, want a refusal naming cluster.context", err)
	}
}

// No value reaches the operator's terminal: the plan names keys and sources, and
// the printer must not be the place that leaks one.
func TestPrintSecretsPlanPrintsNoValue(t *testing.T) {
	var out bytes.Buffer
	printSecretsPlan(&out, "staging", gitops.RenderedEnvironment{Modules: []string{"wiki"}}, &deploysecrets.Plan{
		Store: "fake",
		Secrets: []deploysecrets.SecretPlan{{
			RemoteKey: "wiki-backend", Services: []string{"wiki/backend"}, Exists: true, HasVersion: true, Read: true,
			Properties: []deploysecrets.PropertyPlan{{Property: "TOKEN", Action: deploysecrets.ActionKeep, Source: "stored"}},
		}},
	})
	if strings.Contains(out.String(), "s3cr3t") {
		t.Fatal("the printer rendered a value")
	}
	if !strings.Contains(out.String(), "wiki-backend") || !strings.Contains(out.String(), "TOKEN") {
		t.Errorf("the plan does not name the key and property:\n%s", out.String())
	}
}

type outputSafetyStore struct {
	document map[string]string
	writes   int
}

func (*outputSafetyStore) Name() string { return "test" }
func (store *outputSafetyStore) Describe(context.Context, string) (deploysecrets.Description, error) {
	return deploysecrets.Description{Exists: store.document != nil, HasVersion: store.document != nil}, nil
}
func (store *outputSafetyStore) Read(context.Context, string) (map[string]string, error) {
	return store.document, nil
}
func (store *outputSafetyStore) Write(context.Context, string, map[string]string, bool) error {
	store.writes++
	return nil
}

func TestSecretsCommandRefusesMissingNoOpBeforeConfirmation(t *testing.T) {
	store := &outputSafetyStore{}
	rendered := gitops.RenderedEnvironment{Secrets: []gitops.RenderedServiceSecret{{RemoteKey: "remote", Services: []string{"app/api"}, Properties: []gitops.RenderedSecretProperty{{Property: "required", Keys: []string{"EXTERNAL_KEY"}}}}}}
	plan, err := deploysecrets.Build(context.Background(), &deploysecrets.Inputs{Rendered: rendered, Store: store, ReadPayloads: true})
	if err != nil {
		t.Fatal("could not construct missing-value plan")
	}
	confirmed := false
	confirm := func() bool { confirmed = true; return true }
	if err = finishSecretsPlan(context.Background(), plan, store, false, false, confirm); err == nil {
		t.Fatal("command accepted unresolved no-op plan")
	}
	if confirmed || store.writes != 0 {
		t.Fatal("incomplete plan prompted or wrote")
	}
	if err = finishSecretsPlan(context.Background(), plan, store, true, false, confirm); err != nil {
		t.Fatal("dry run should report the incomplete plan")
	}
	if err = finishSecretsPlan(context.Background(), plan, store, false, true, confirm); err != nil {
		t.Fatal("explicit allow-missing should permit a no-op")
	}
}
