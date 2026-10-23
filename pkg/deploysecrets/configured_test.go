package deploysecrets

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/core/resources"
)

// The key the operator's own file supplies, in both scopes. Derived through
// core's encoding in configuredSecretKey, so these are the names a render
// records as `secretKey` rather than strings that were true once.
const (
	configuredGroup   = "identity"
	configuredKeyName = "OAUTH_CLIENT_SECRET"
)

// writeSecretConfiguration writes one `configurations/<profile>/<group>.secret.env`
// the way `codefly config set` does, and returns its workspace-relative path —
// the path the plan must name as the source.
func writeSecretConfiguration(t *testing.T, scopeDir, profile, group string, entries ...string) string {
	t.Helper()
	dir := filepath.Join(scopeDir, "configurations", profile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, group+".secret.env")
	if err := os.WriteFile(path, []byte(strings.Join(entries, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// configuredIn reads a scope directory the way `deploy secrets` reads the
// workspace: through readConfiguredScope, so every test below resolves its key
// from a real file rather than from a hand-built index.
func configuredIn(t *testing.T, root, scopeDir, profile, origin string) map[string]ConfiguredValue {
	t.Helper()
	index := map[string]ConfiguredValue{}
	if err := readConfiguredScope(index, root, scopeDir, profile, origin); err != nil {
		t.Fatal(err)
	}
	return index
}

// renderedConfigured is one rendered ExternalSecret reading one key, filed under
// a property of its own name.
func renderedConfigured(key string) gitops.RenderedEnvironment {
	return gitops.RenderedEnvironment{
		Modules: []string{"host"},
		Secrets: []gitops.RenderedServiceSecret{{
			Store:      environments.EnvironmentSecretStoreReference{Name: "cell-secrets", Kind: "ClusterSecretStore"},
			RemoteKey:  "host-accounts",
			Services:   []string{"host/accounts"},
			Properties: filed(key),
		}},
	}
}

// The source this issue adds: a property whose secret key no remote key holds,
// and which the environment's own `<group>.secret.env` holds a value for, is
// planned as `configured` from that file — and an apply writes it.
//
// This is the whole operator path. Before it, the same property reported
// `require`, and the only way to make the workload startable was to type the
// value into the store's console or `gcloud` by hand — for a value the operator
// had already given Codefly, in the file a local run reads.
func TestAConfiguredPropertyIsSeededFromTheEnvironmentsOwnFile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := writeSecretConfiguration(t, root, "staging", configuredGroup, configuredKeyName+"=external-client-secret")
	configured := configuredIn(t, root, root, "staging", resources.ConfigurationWorkspace)

	key := configuredSecretKey(resources.ConfigurationWorkspace, configuredGroup, configuredKeyName)
	if _, held := configured[key]; !held {
		t.Fatalf("the file supplies no value for %s; it indexed %v", key, slices.Sorted(maps.Keys(configured)))
	}

	store := newFakeStore(map[string]map[string]string{})
	plan, err := Build(ctx, &Inputs{
		Rendered: renderedConfigured(key), Store: store, ReadPayloads: true,
		Services: []string{"host/accounts"}, Configured: configured,
	})
	if err != nil {
		t.Fatal(err)
	}
	property := propertyPlan(t, plan, "host-accounts", key)
	if property.Action != ActionConfigured {
		t.Errorf("a property the environment's own configuration holds = %s, want %s", property.Action, ActionConfigured)
	}
	// The source names the FILE, which is what --dry-run reports, and names
	// nothing else: a plan carries paths and key names, never a value.
	relative, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatal(err)
	}
	if property.Source != "configured in "+relative {
		t.Errorf("Source = %q, want it to name %s", property.Source, relative)
	}
	if strings.Contains(property.Source, "external-client-secret") {
		t.Error("the plan's source carries the value")
	}
	if got := plan.Required(); len(got) != 0 {
		t.Errorf("Required() = %v, want nothing: the configuration file is what supplies the value", got)
	}
	if got := plan.Changes(); !slices.Equal(got, []string{"host-accounts"}) {
		t.Errorf("Changes() = %v, want the remote key to be written", got)
	}
	if _, err = plan.Apply(ctx, store, false); err != nil {
		t.Fatal(err)
	}
	if got := store.documents["host-accounts"][key]; got != "external-client-secret" {
		t.Errorf("apply wrote %q, want the configured value", got)
	}
}

// A stored value wins and is never rotated, exactly as before: the file is
// consulted for a property nothing holds, not to replace one that is held.
func TestAStoredValueWinsOverTheConfiguredOneAndIsNotRotated(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeSecretConfiguration(t, root, "staging", configuredGroup, configuredKeyName+"=external-client-secret")
	key := configuredSecretKey(resources.ConfigurationWorkspace, configuredGroup, configuredKeyName)

	store := newFakeStore(map[string]map[string]string{"host-accounts": {key: "external-client-secret"}})
	plan, err := Build(ctx, &Inputs{
		Rendered: renderedConfigured(key), Store: store, ReadPayloads: true,
		Services:   []string{"host/accounts"},
		Configured: configuredIn(t, root, root, "staging", resources.ConfigurationWorkspace),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := propertyPlan(t, plan, "host-accounts", key); got.Action != ActionKeep {
		t.Errorf("a stored property = %s, want %s: a stored value is never rotated", got.Action, ActionKeep)
	}
	if got := plan.Changes(); len(got) != 0 {
		t.Errorf("Changes() = %v, want nothing written", got)
	}
}

// A stored value that DIFFERS from the configured one is refused, like two
// remote keys disagreeing today. Keeping it would leave the deployed workload
// on one value and every local run on the other, and the plan would say `keep`
// — the one reading that tells an operator nothing is wrong.
func TestAStoredValueDifferingFromTheConfiguredOneRefuses(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := writeSecretConfiguration(t, root, "staging", configuredGroup, configuredKeyName+"=rotated-client-secret")
	key := configuredSecretKey(resources.ConfigurationWorkspace, configuredGroup, configuredKeyName)

	_, err := Build(ctx, &Inputs{
		Rendered:     renderedConfigured(key),
		Store:        newFakeStore(map[string]map[string]string{"host-accounts": {key: "stale-client-secret"}}),
		ReadPayloads: true, Services: []string{"host/accounts"},
		Configured: configuredIn(t, root, root, "staging", resources.ConfigurationWorkspace),
	})
	if err == nil {
		t.Fatal("a stored value differing from the configured one was accepted")
	}
	relative, relErr := filepath.Rel(root, path)
	if relErr != nil {
		t.Fatal(relErr)
	}
	for _, want := range []string{key, "host-accounts", relative, "never rotated"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Build error = %q, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "client-secret") {
		t.Error("the refusal carries a value")
	}
}

// A tracked placeholder file is the committed shape that declares which keys an
// environment needs and holds no value for any of them. It keeps contributing
// keys and no values: the property stays `require`, so an operator is still
// told to supply it rather than having an empty string written to the store.
func TestATrackedEmptyPlaceholderSuppliesNoValue(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeSecretConfiguration(t, root, "staging", configuredGroup, configuredKeyName+"=")
	key := configuredSecretKey(resources.ConfigurationWorkspace, configuredGroup, configuredKeyName)

	configured := configuredIn(t, root, root, "staging", resources.ConfigurationWorkspace)
	if len(configured) != 0 {
		t.Fatalf("a placeholder supplied %d values, want none", len(configured))
	}
	store := newFakeStore(map[string]map[string]string{})
	plan, err := Build(ctx, &Inputs{
		Rendered: renderedConfigured(key), Store: store, ReadPayloads: true,
		Services: []string{"host/accounts"}, Configured: configured,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := propertyPlan(t, plan, "host-accounts", key); got.Action != ActionRequire {
		t.Errorf("a property whose only declaration is an empty placeholder = %s, want %s", got.Action, ActionRequire)
	}
	if got := plan.Required(); len(got) != 1 {
		t.Errorf("Required() = %v, want the one property the operator must supply", got)
	}
	if got := plan.Changes(); len(got) != 0 {
		t.Errorf("Changes() = %v, want nothing: an empty value is not a value", got)
	}
}

// A configured value and another remote key holding the same configuration
// value disagreeing is refused, on the rule that already covers two remote
// keys: one configuration value cannot be two.
func TestAPropagatedValueDifferingFromTheConfiguredOneRefuses(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeSecretConfiguration(t, root, "staging", configuredGroup, configuredKeyName+"=configured-value")
	key := configuredSecretKey(resources.ConfigurationWorkspace, configuredGroup, configuredKeyName)

	store := environments.EnvironmentSecretStoreReference{Name: "cell-secrets", Kind: "ClusterSecretStore"}
	rendered := gitops.RenderedEnvironment{Modules: []string{"host", "notes"}, Secrets: []gitops.RenderedServiceSecret{
		{Store: store, RemoteKey: "host-accounts", Services: []string{"host/accounts"}, Properties: filed(key)},
		{Store: store, RemoteKey: "notes-backend", Services: []string{"notes/backend"}, Properties: filed(key)},
	}}
	_, err := Build(ctx, &Inputs{
		Rendered:     rendered,
		Store:        newFakeStore(map[string]map[string]string{"host-accounts": {key: "propagated-value"}}),
		ReadPayloads: true, Services: []string{"host/accounts", "notes/backend"},
		Configured: configuredIn(t, root, root, "staging", resources.ConfigurationWorkspace),
	})
	if err == nil {
		t.Fatal("a holder differing from the configured value was accepted")
	}
	if !strings.Contains(err.Error(), "one configuration value cannot be two") {
		t.Errorf("Build error = %q, want the configuration-value disagreement", err)
	}
}

// A key the environment declares random AND the configuration holds is seeded
// from the file. A generator mints what nothing supplies — the same reason a
// stored value is kept rather than regenerated — so a value the operator chose
// is never replaced by a random one.
func TestAConfiguredValueIsPreferredToADeclaredGenerator(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeSecretConfiguration(t, root, "staging", "internal-auth", "CODEFLY_INTERNAL_TOKEN=chosen-token")
	key := configuredSecretKey(resources.ConfigurationWorkspace, "internal-auth", "CODEFLY_INTERNAL_TOKEN")

	store := newFakeStore(map[string]map[string]string{})
	plan, err := Build(ctx, &Inputs{
		Rendered: renderedConfigured(key), Store: store, ReadPayloads: true,
		Services: []string{"host/accounts"},
		Generators: []environments.EnvironmentSecretGenerator{{
			Scope: environments.SecretGeneratorScopeWorkspace, Configuration: "internal-auth",
			Keys: []string{"CODEFLY_INTERNAL_TOKEN"},
		}},
		Configured: configuredIn(t, root, root, "staging", resources.ConfigurationWorkspace),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := propertyPlan(t, plan, "host-accounts", key); got.Action != ActionConfigured {
		t.Errorf("a configured key the environment also declares random = %s, want %s", got.Action, ActionConfigured)
	}
	if _, err = plan.Apply(ctx, store, false); err != nil {
		t.Fatal(err)
	}
	if got := store.documents["host-accounts"][key]; got != "chosen-token" {
		t.Errorf("apply wrote %q, want the configured value rather than a generated one", got)
	}
}

// The service scope, and the full read a run of the command performs: a real
// workspace, a workspace-scoped file and a service-scoped one, each indexed
// under the carrier core builds for its own scope.
func TestReadConfiguredIndexesBothScopesOfTheWorkspace(t *testing.T) {
	ctx, workspace, root := configuredWorkspaceFixture(t)
	writeSecretConfiguration(t, root, "staging", configuredGroup, configuredKeyName+"=workspace-value")
	serviceDir := filepath.Join(root, "modules", "platform", "services", "api")
	writeSecretConfiguration(t, serviceDir, "staging", "runtime", "SCHEDULER_TOKEN=service-value")

	configured, err := ReadConfigured(ctx, workspace, &environments.Environment{Name: "staging"})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		configuredSecretKey(resources.ConfigurationWorkspace, configuredGroup, configuredKeyName): filepath.Join("configurations", "staging", configuredGroup+".secret.env"),
		configuredSecretKey("platform/api", "runtime", "SCHEDULER_TOKEN"):                         filepath.Join("modules", "platform", "services", "api", "configurations", "staging", "runtime.secret.env"),
	} {
		value, held := configured[key]
		if !held {
			t.Errorf("ReadConfigured holds no %s; it indexed %v", key, slices.Sorted(maps.Keys(configured)))
			continue
		}
		if value.File != want {
			t.Errorf("%s came from %q, want %q", key, value.File, want)
		}
	}
	// The workspace carrier and the service carrier are different names, so one
	// scope never answers for the other.
	if len(configured) != 2 {
		t.Errorf("ReadConfigured indexed %v, want exactly the two configured values", slices.Sorted(maps.Keys(configured)))
	}
}

// Core folds a configuration name into its carrier, so two files whose names
// differ only by a separator are one secret key. Writing whichever sorted first
// would seed the store by an accident of filesystem order, so both are named
// and the read stops.
func TestTwoConfigurationNamesSpellingOneCarrierAreRefused(t *testing.T) {
	root := t.TempDir()
	writeSecretConfiguration(t, root, "staging", "datasource-oauth", configuredKeyName+"=one")
	writeSecretConfiguration(t, root, "staging", "datasource_oauth", configuredKeyName+"=another")

	err := readConfiguredScope(map[string]ConfiguredValue{}, root, root, "staging", resources.ConfigurationWorkspace)
	if err == nil {
		t.Fatal("two configuration names spelling one carrier were accepted")
	}
	for _, want := range []string{"datasource-oauth.secret.env", "datasource_oauth.secret.env", "one carrier"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("readConfiguredScope error = %q, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "another") {
		t.Error("the refusal carries a value")
	}
}

// A reference-only manifest is never a source: its values are `op://…`
// references resolved against a backend, and writing one to the store would
// store the reference instead of the value.
func TestAReferenceOnlyManifestIsNotAConfiguredValue(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "configurations", "staging")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vault.secret.ref.env"), []byte("TOKEN=op://vault/item/field\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configured := configuredIn(t, root, root, "staging", resources.ConfigurationWorkspace)
	if len(configured) != 0 {
		t.Errorf("a reference-only manifest supplied %v, want nothing", slices.Sorted(maps.Keys(configured)))
	}
}

// configuredWorkspaceFixture writes a real Codefly workspace with one module
// and one service: ReadConfigured walks real manifests to find the service
// scopes, so nothing here is stubbed.
func configuredWorkspaceFixture(t *testing.T) (context.Context, *resources.Workspace, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"workspace.codefly.yaml": `name: configured-fixture
layout: modules
modules:
    - name: platform
environments:
    - name: staging
`,
		"modules/platform/module.codefly.yaml": `kind: module
name: platform
services:
    - name: api
`,
		"modules/platform/services/api/service.codefly.yaml": `kind: service
name: api
version: 0.0.0
module: platform
agent:
    kind: codefly:service
    name: go-grpc
    version: 0.1.24
    publisher: codefly.dev
`,
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	workspace, err := resources.LoadWorkspaceFromDir(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, workspace, dir
}
