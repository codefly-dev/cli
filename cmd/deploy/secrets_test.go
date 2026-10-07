package deploy

import (
	"bytes"
	"context"
	"strings"
	"sync"
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

// Each backend plan writes one store. A mixed group is rejected rather than
// seeding whichever store it saw first.
func TestResolveSecretStoreRefusesTwoStores(t *testing.T) {
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

const routedConfigurationKey = "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__INTERNAL_AUTH__TOKEN"

type backendRoutingStore struct {
	mutex    sync.Mutex
	name     string
	document map[string]string
	reads    []string
	written  map[string]string
}

func (store *backendRoutingStore) Name() string { return store.name }
func (store *backendRoutingStore) Describe(context.Context, string) (deploysecrets.Description, error) {
	return deploysecrets.Description{Exists: true, HasVersion: true}, nil
}
func (store *backendRoutingStore) Read(_ context.Context, key string) (map[string]string, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	store.reads = append(store.reads, key)
	if key == "destination" {
		return map[string]string{}, nil
	}
	return store.document, nil
}
func (store *backendRoutingStore) Write(_ context.Context, key string, document map[string]string, _ bool) error {
	store.written = document
	return nil
}

func TestSecretPlansRouteReadsAndWritesToEachBackend(t *testing.T) {
	ctx := context.Background()
	stores := map[string]*backendRoutingStore{
		"database": {name: "database", document: map[string]string{"token": "database-secret-value"}},
		"identity": {name: "identity", document: map[string]string{"token": "identity-secret-value"}},
	}
	rendered := gitops.RenderedEnvironment{Modules: []string{"app"}}
	for _, name := range []string{"identity", "database"} {
		for _, remote := range []string{"source", "destination"} {
			property := "token"
			if remote == "destination" {
				property = "copy"
			}
			rendered.Secrets = append(rendered.Secrets, gitops.RenderedServiceSecret{
				Store:     environments.EnvironmentSecretStoreReference{Name: name, Kind: "ClusterSecretStore"},
				RemoteKey: remote, Services: []string{"app/api"}, Properties: []gitops.RenderedSecretProperty{{Property: property, Keys: []string{routedConfigurationKey}}},
			})
		}
	}
	plans, err := planSecretStores(ctx, &deploysecrets.Inputs{Rendered: rendered, ReadPayloads: true}, func(group gitops.RenderedEnvironment) (deploysecrets.Store, error) {
		return stores[group.Secrets[0].Store.Name], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].plan.Store != "database" || plans[1].plan.Store != "identity" {
		t.Fatalf("unstable backend ordering")
	}
	var output bytes.Buffer
	for _, planned := range plans {
		printSecretsPlan(&output, "staging", planned.rendered, planned.plan)
	}
	if strings.Contains(output.String(), "secret-value") {
		t.Fatal("plan printed secret payload")
	}
	if err := finishSecretPlans(ctx, plans, false, false, func(plannedSecretStore) bool { return true }); err != nil {
		t.Fatal(err)
	}
	for _, store := range stores {
		if len(store.reads) != 2 || store.written["copy"] != store.document["token"] {
			t.Fatalf("backend %s did not retain its own value", store.name)
		}
	}
}

func TestSecretStoreGroupingRespectsNamespace(t *testing.T) {
	rendered := gitops.RenderedEnvironment{}
	for _, kind := range []string{"SecretStore", "ClusterSecretStore"} {
		for _, namespace := range []string{"a", "b"} {
			rendered.Secrets = append(rendered.Secrets, gitops.RenderedServiceSecret{Store: environments.EnvironmentSecretStoreReference{Name: "store", Kind: kind}, Namespace: namespace, RemoteKey: "key"})
		}
	}
	groups := groupSecretStores(rendered)
	if len(groups) != 3 || len(groups[0].Secrets) != 2 || groups[1].Secrets[0].Namespace != "a" || groups[2].Secrets[0].Namespace != "b" {
		t.Fatalf("grouped wrong store scope: %+v", groups)
	}
}

func TestSecretPlansValidateAllBackendsBeforeWriting(t *testing.T) {
	ctx := context.Background()
	store := &backendRoutingStore{name: "store", document: map[string]string{"present": "never-print-this"}}
	var plans []plannedSecretStore
	for _, complete := range []bool{true, false} {
		rendered := gitops.RenderedEnvironment{Secrets: []gitops.RenderedServiceSecret{
			{RemoteKey: "source", Properties: []gitops.RenderedSecretProperty{{Property: "present", Keys: []string{routedConfigurationKey}}}},
			{RemoteKey: "destination", Properties: []gitops.RenderedSecretProperty{{Property: "copy", Keys: []string{routedConfigurationKey}}}},
		}}
		if !complete {
			rendered.Secrets[1].Properties[0].Keys = []string{"REQUIRED"}
		}
		plan, err := deploysecrets.Build(ctx, &deploysecrets.Inputs{Rendered: rendered, Store: store, ReadPayloads: true})
		if err != nil {
			t.Fatal(err)
		}
		plans = append(plans, plannedSecretStore{store: store, plan: plan})
	}
	confirmed := false
	err := finishSecretPlans(ctx, plans, false, false, func(plannedSecretStore) bool { confirmed = true; return true })
	if err == nil || confirmed || store.written != nil {
		t.Fatal("an incomplete later backend allowed confirmation or writes")
	}
}

func TestSecretPlansKeepImportedCredentialsInTheirSourceBackend(t *testing.T) {
	generator := environments.EnvironmentSecretGenerator{Scope: "workspace", Configuration: "internal-auth", Keys: []string{"token"}}
	generatedKey := generator.StoredKeys(nil)[0]
	identity := &backendRoutingStore{name: "shared-identity-project", document: map[string]string{"client_secret": "external-user-supplied-secret"}}
	data := &backendRoutingStore{name: "cell-data-project", document: map[string]string{}}
	rendered := gitops.RenderedEnvironment{Secrets: []gitops.RenderedServiceSecret{
		{Store: environments.EnvironmentSecretStoreReference{Name: "identity", Kind: "ClusterSecretStore"}, RemoteKey: "credentials", Properties: []gitops.RenderedSecretProperty{{Property: "client_secret", Keys: []string{"USER_SUPPLIED_CLIENT_SECRET"}}}},
		{Store: environments.EnvironmentSecretStoreReference{Name: "data", Kind: "ClusterSecretStore"}, RemoteKey: "app", Properties: []gitops.RenderedSecretProperty{{Property: "internal_token", Keys: []string{generatedKey}}}},
	}}
	plans, err := planSecretStores(t.Context(), &deploysecrets.Inputs{Rendered: rendered, ReadPayloads: true, Generators: []environments.EnvironmentSecretGenerator{generator}}, func(group gitops.RenderedEnvironment) (deploysecrets.Store, error) {
		if group.Secrets[0].Store.Name == "identity" {
			return identity, nil
		}
		return data, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := finishSecretPlans(t.Context(), plans, false, false, func(plannedSecretStore) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if identity.written != nil || len(data.written) != 1 || data.written["internal_token"] == "" {
		t.Fatal("imported credentials were changed or copied to another project")
	}
}
