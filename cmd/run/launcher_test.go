package run

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/run/identity"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/workloadidentity"
	"github.com/codefly-dev/core/workloadidentity/native"
	"github.com/stretchr/testify/require"
)

// TestEveryIdentityWorkloadIsIssuedItsCredentialAtProjection: a module runs
// as each of its identity services, and each is issued its credential the
// moment it is projected — none is withheld until a delivery — naming its
// own account and its own declared container, and nothing about a build.
func TestEveryIdentityWorkloadIsIssuedItsCredentialAtProjection(t *testing.T) {
	ctx := context.Background()
	workspace, env := writeHostedNativeWorkspace(t)
	workspace = composeService(t, workspace, "model", "worker")
	identities, err := gitops.ResolveNativeIdentities(ctx, workspace, env)
	require.NoError(t, err)
	vouch, err := identity.Start(env.Host.TrustDomain, env.Host.Audience, t.TempDir())
	require.NoError(t, err)
	cluster := &launcher{vouch: vouch, workspace: workspace, env: env, identities: identities}
	cluster.declare(identities)
	attester, err := native.NewAttester(vouch.Anchor())
	require.NoError(t, err)
	for _, unique := range []string{"model/model", "model/worker"} {
		identity, ok := cluster.identity(unique)
		require.True(t, ok, unique)
		credential, err := cluster.project(unique, identity)
		require.NoError(t, err)
		token, err := os.ReadFile(credential.File)
		require.NoError(t, err, "%s holds its credential from projection", unique)
		attested, err := attester.Attest(ctx, string(token), env.Host.Audience)
		require.NoError(t, err)
		require.Equal(t, workloadidentity.Workload{TrustDomain: env.Host.TrustDomain, Module: "model", Service: identity.Service}, attested.Workload.Declared(),
			"the credential names the declared workload and nothing of a cell")
		require.NotEmpty(t, attested.Workload.Instance)
		require.Len(t, attested.Facts.Containers, 1)
		require.Equal(t, identity.Service, attested.Facts.Containers[0].Name, "the credential names the workload's own declared container")
		again, err := cluster.project(unique, identity)
		require.NoError(t, err)
		require.Equal(t, credential, again, "a workload is projected once")
	}
	t.Run("the run vouches for its carriers and the observer, and nothing else", func(t *testing.T) {
		for _, workload := range []workloadidentity.Workload{vouch.Workload("model", workloadidentity.DeliveryService), vouch.Observer()} {
			token, err := vouch.Issue(workload)
			require.NoError(t, err)
			_, err = attester.Attest(ctx, token, env.Host.Audience)
			require.NoError(t, err)
		}
		_, err := vouch.Issue(vouch.Workload("elsewhere", "api"))
		require.ErrorContains(t, err, "did not declare elsewhere/api")
	})
}

// The identity workloads a module's manifest lists are the ones that reached
// Running, of that module, by service.
func TestStartedIdentitiesAreTheRunningWorkloadsOfTheModule(t *testing.T) {
	cluster := &launcher{
		identities: map[string]gitops.NativeIdentity{
			"model/worker": {Module: "model", Service: "worker"}, "model/model": {Module: "model", Service: "model"},
			"model/idle": {Module: "model", Service: "idle"}, "ledger/store": {Module: "ledger", Service: "store"},
		},
		running: map[string]bool{"model/worker": true, "model/model": true, "ledger/store": true, "gone/api": true},
	}
	require.Equal(t, []gitops.NativeIdentity{{Module: "model", Service: "model"}, {Module: "model", Service: "worker"}}, cluster.startedIdentities("model"))
	require.Empty(t, cluster.startedIdentities("other"))
}

// writeHostedNativeWorkspace lays down a workspace composing module "model"
// with a module-identity service, in an environment that names a host, and
// returns it with the environment a native run selects. Another module can
// be composed into it later with composeModule.
func writeHostedNativeWorkspace(t *testing.T) (*resources.Workspace, *environments.Environment) {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		resources.WorkspaceConfigurationName: `name: acme
layout: modules
modules:
  - name: model
  - name: ledger
environments:
  - name: local
    namespace: acme
    host:
      coordinate: example/local/dev
      component: host
      domain: acme
      audience: accounts
      trust_domain: cluster.local
      delivery: model/model/rest
`,
		filepath.Join("modules", "model", resources.ModuleConfigurationName):                        "kind: module\nname: model\nservices:\n  - name: model\n",
		filepath.Join("modules", "ledger", resources.ModuleConfigurationName):                       "kind: module\nname: ledger\nservices:\n  - name: store\n",
		filepath.Join("modules", "ledger", "services", "store", resources.ServiceConfigurationName): "kind: service\nname: store\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\n",
		filepath.Join("modules", "model", "services", "model", resources.ServiceConfigurationName): `kind: service
name: model
version: 0.0.0
module-identity: true
agent:
  kind: runtime::service
  name: python-fastapi
  version: 0.0.112
  publisher: codefly.dev
endpoints:
  - name: rest
    api: rest
    visibility: internal
`,
	}
	for relative, content := range files {
		full := filepath.Join(root, relative)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	require.NoError(t, err)
	env, err := environments.Select(workspace, "local")
	require.NoError(t, err)
	return workspace, env
}

// composeModule adds a module with one module-identity service to the
// workspace on disk and returns the workspace reloaded, as a running
// workspace command would reload it.
func composeModule(t *testing.T, workspace *resources.Workspace, name string) *resources.Workspace {
	t.Helper()
	configuration := filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName)
	config, err := os.ReadFile(configuration)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configuration, []byte(strings.Replace(string(config), "modules:\n", "modules:\n  - name: "+name+"\n", 1)), 0o644))
	for relative, content := range map[string]string{
		filepath.Join("modules", name, resources.ModuleConfigurationName):                     "kind: module\nname: " + name + "\nservices:\n  - name: api\n",
		filepath.Join("modules", name, "services", "api", resources.ServiceConfigurationName): "kind: service\nname: api\nversion: 0.0.0\nmodule-identity: true\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\n",
	} {
		full := filepath.Join(workspace.Dir(), relative)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	reloaded, err := resources.LoadWorkspaceFromDir(context.Background(), workspace.Dir())
	require.NoError(t, err)
	return reloaded
}

// composeService adds a module-identity service to a composed module on disk
// and returns the workspace reloaded.
func composeService(t *testing.T, workspace *resources.Workspace, module, service string) *resources.Workspace {
	t.Helper()
	configuration := filepath.Join(workspace.Dir(), "modules", module, resources.ModuleConfigurationName)
	config, err := os.ReadFile(configuration)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configuration, []byte(string(config)+"  - name: "+service+"\n"), 0o644))
	full := filepath.Join(workspace.Dir(), "modules", module, "services", service, resources.ServiceConfigurationName)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte("kind: service\nname: "+service+"\nversion: 0.0.0\nmodule-identity: true\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\n"), 0o644))
	reloaded, err := resources.LoadWorkspaceFromDir(context.Background(), workspace.Dir())
	require.NoError(t, err)
	return reloaded
}

// A component composed after the run started is admitted into the running
// composition: its identity is planned by the rule attach used and added, the
// components already known neither change nor disappear, and the run's
// authority is untouched — the host keeps its process, and the newcomer is
// delivered like any workload reaching Running.
func TestANativeRunAdmitsAnUnseenComponentWithoutReplacingItsAuthority(t *testing.T) {
	ctx := context.Background()
	workspace, env := writeHostedNativeWorkspace(t)
	identities, err := gitops.ResolveNativeIdentities(ctx, workspace, env)
	require.NoError(t, err)
	require.Len(t, identities, 1)
	cluster := &launcher{workspace: workspace, env: env, identities: identities, hostIdentityAnchor: "/run/host/identity-anchor.json"}
	before := cluster.hostIdentityAnchor

	reloaded := composeModule(t, workspace, "wiki")
	added, err := cluster.admit(ctx, reloaded)
	require.NoError(t, err)
	require.Equal(t, []string{"wiki/api"}, added)
	identity, ok := cluster.identity("wiki/api")
	require.True(t, ok)
	require.Equal(t, gitops.NativeIdentity{Module: "wiki", Service: "api"}, identity)
	require.Same(t, reloaded, cluster.composed(), "the run composes the reloaded workspace from now on")
	_, stillKnown := cluster.identity("model/model")
	require.True(t, stillKnown)
	require.Nil(t, cluster.vouch, "admission starts no issuer")
	require.Equal(t, before, cluster.hostIdentityAnchor, "the host's identity anchor is untouched")

	again, err := cluster.admit(ctx, reloaded)
	require.NoError(t, err)
	require.Empty(t, again, "admitting the same composition twice adds nothing")

	t.Run("a known principal is never replaced", func(t *testing.T) {
		changed := &launcher{workspace: workspace, env: env, identities: map[string]gitops.NativeIdentity{
			"model/model": {Module: "elsewhere", Service: "model"},
		}}
		_, err := changed.admit(ctx, reloaded)
		require.ErrorContains(t, err, "model/model runs as elsewhere/model")
		require.ErrorContains(t, err, "never replaced")
		_, admitted := changed.identity("wiki/api")
		require.False(t, admitted, "a refused admission leaves the run as it was")
		require.Same(t, workspace, changed.composed())
	})
	t.Run("a known component is never withdrawn", func(t *testing.T) {
		withdrawn, err := resources.LoadWorkspaceFromDir(ctx, writeNonGoWorkloadDir(t, "other"))
		require.NoError(t, err)
		_, err = cluster.admit(ctx, withdrawn)
		require.ErrorContains(t, err, "no longer composes model/model")
		require.ErrorContains(t, err, "never withdraws")
		require.Same(t, reloaded, cluster.composed())
	})
}

// writeNonGoWorkloadDir lays down a workspace composing one module of the
// given name with one module-identity service, in a hosted environment,
// returning the directory.
func writeNonGoWorkloadDir(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		resources.WorkspaceConfigurationName:                                                  "name: acme\nlayout: modules\nmodules:\n  - name: " + name + "\nenvironments:\n  - name: local\n    namespace: acme\n    host:\n      coordinate: example/local/dev\n      component: host\n      domain: acme\n      audience: accounts\n      trust_domain: cluster.local\n      delivery: " + name + "/api/rest\n",
		filepath.Join("modules", name, resources.ModuleConfigurationName):                     "kind: module\nname: " + name + "\nservices:\n  - name: api\n",
		filepath.Join("modules", name, "services", "api", resources.ServiceConfigurationName): "kind: service\nname: api\nversion: 0.0.0\nmodule-identity: true\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\nendpoints:\n  - name: rest\n    api: rest\n    visibility: internal\n",
	}
	for relative, content := range files {
		full := filepath.Join(root, relative)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	return root
}

func TestNativeAudienceMappingsAndVerifierIsolation(t *testing.T) {
	workspace, env := writeHostedNativeWorkspace(t)
	env.NativeIdentity = &environments.EnvironmentNativeIdentity{
		Projections: []environments.NativeIdentityProjection{
			{Service: "model/model", Audiences: []string{env.Host.Audience, "other-audience"}},
		}, Verifiers: []string{"ledger/store"},
	}
	plan, err := gitops.ResolveNativeIdentityPlan(t.Context(), workspace, env)
	require.NoError(t, err)
	vouch, err := identity.Start(env.Host.TrustDomain, env.Host.Audience, t.TempDir())
	require.NoError(t, err)
	cluster := &launcher{vouch: vouch, workspace: workspace, env: env, identities: plan.Identities,
		projections: plan.Projections, verifiers: plan.Verifiers, hostIdentityAnchor: filepath.Join(t.TempDir(), "anchor.json"), running: map[string]bool{}}
	cluster.declare(plan.Identities)
	cluster.declareProjections()
	require.NoError(t, vouch.WriteAnchor(cluster.hostIdentityAnchor))
	t.Setenv(native.AnchorEnvironment, cluster.hostIdentityAnchor)
	attester, err := native.LoadAttester()
	require.NoError(t, err)
	service, err := workspace.LoadService(t.Context(), &resources.ServiceWithModule{Module: "model", Name: "model"})
	require.NoError(t, err)
	overrides, err := cluster.runtimeOverrides(service)
	require.NoError(t, err)
	require.NotContains(t, overrides, native.AnchorEnvironment)
	var mapping map[string]string
	require.NoError(t, json.Unmarshal([]byte(overrides[environments.NativeIdentityCredentials]), &mapping))
	require.Len(t, mapping, 2)
	require.Equal(t, overrides[workloadTokenFileVariable], mapping[env.Host.Audience])
	for audience, path := range mapping {
		token, err := os.ReadFile(path)
		require.NoError(t, err)
		_, err = attester.Attest(t.Context(), string(token), audience)
		require.NoError(t, err)
	}
	verifier, err := workspace.LoadService(t.Context(), &resources.ServiceWithModule{Module: "ledger", Name: "store"})
	require.NoError(t, err)
	overrides, err = cluster.runtimeOverrides(verifier)
	require.NoError(t, err)
	require.Equal(t, map[string]string{native.AnchorEnvironment: cluster.hostIdentityAnchor}, overrides)
	unrelated := &resources.Service{Name: "other"}
	unrelated.WithModule("example")
	overrides, err = cluster.runtimeOverrides(unrelated)
	require.NoError(t, err)
	require.Empty(t, overrides)
	require.NoError(t, cluster.forget("model/model"))
	require.NoError(t, vouch.Refresh())
	for _, path := range mapping {
		require.NoFileExists(t, path)
	}
}

func TestNativeProjectionDoesNotOptIntoModuleDelivery(t *testing.T) {
	workspace, env := writeHostedNativeWorkspace(t)
	env.NativeIdentity = &environments.EnvironmentNativeIdentity{
		Projections: []environments.NativeIdentityProjection{{Service: "ledger/store", Audiences: []string{"second"}}},
	}
	plan, err := gitops.ResolveNativeIdentityPlan(t.Context(), workspace, env)
	require.NoError(t, err)
	require.NotContains(t, plan.Identities, "ledger/store")
	require.Equal(t, []string{"second"}, plan.Projections["ledger/store"])
	vouch, err := identity.Start(env.Host.TrustDomain, env.Host.Audience, t.TempDir())
	require.NoError(t, err)
	cluster := &launcher{vouch: vouch, env: env, identities: plan.Identities, projections: plan.Projections}
	cluster.declare(plan.Identities)
	cluster.declareProjections()
	service, err := workspace.LoadService(t.Context(), &resources.ServiceWithModule{Module: "ledger", Name: "store"})
	require.NoError(t, err)
	overrides, err := cluster.runtimeOverrides(service)
	require.NoError(t, err)
	require.Len(t, overrides, 1)
	require.Contains(t, overrides, environments.NativeIdentityCredentials)
	require.False(t, vouch.Anchor().Vouches(vouch.Workload("ledger", workloadidentity.DeliveryService)))

	env.NativeIdentity.Projections[0].Service = "absent/service"
	_, err = gitops.ResolveNativeIdentityPlan(t.Context(), workspace, env)
	require.ErrorContains(t, err, "unknown service")
	env.NativeIdentity.Projections[0].Service = "ledger/store"
	env.ManagedServices = map[string]environments.EnvironmentManagedService{"ledger/store": {Kind: "external", ExternalName: "example.test"}}
	_, err = gitops.ResolveNativeIdentityPlan(t.Context(), workspace, env)
	require.ErrorContains(t, err, "managed")
}

func TestNativeProjectionFailureIsReturned(t *testing.T) {
	workspace, env := writeHostedNativeWorkspace(t)
	blocked := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocked, []byte("not a directory"), 0o600))
	vouch, err := identity.Start(env.Host.TrustDomain, env.Host.Audience, blocked)
	require.NoError(t, err)
	identities, err := gitops.ResolveNativeIdentities(t.Context(), workspace, env)
	require.NoError(t, err)
	cluster := &launcher{vouch: vouch, env: env, identities: identities}
	cluster.declare(identities)
	service, err := workspace.LoadService(t.Context(), &resources.ServiceWithModule{Module: "model", Name: "model"})
	require.NoError(t, err)
	overrides, err := cluster.runtimeOverrides(service)
	require.ErrorContains(t, err, "project model/model")
	require.Nil(t, overrides)
}

func TestNativeDeclarationChangeRequiresNewRun(t *testing.T) {
	workspace, env := writeHostedNativeWorkspace(t)
	cluster := &launcher{workspace: workspace, env: env}
	file := filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName)
	contents, err := os.ReadFile(file)
	require.NoError(t, err)
	contents = append(contents, []byte("    native-identity:\n      verifiers: [ledger/store]\n")...)
	require.NoError(t, os.WriteFile(file, contents, 0o600))
	reloaded, err := resources.LoadWorkspaceFromDir(t.Context(), workspace.Dir())
	require.NoError(t, err)
	_, err = cluster.admit(t.Context(), reloaded)
	require.ErrorContains(t, err, "native-identity declaration changed")
	require.Same(t, workspace, cluster.workspace)
}

func TestNativeIdentityRefusesIncompatibleAttachBeforeAnyLaunch(t *testing.T) {
	previous := runtimeContext
	t.Cleanup(func() { runtimeContext = previous })
	env := &environments.Environment{Host: &environments.EnvironmentHost{}, NativeIdentity: &environments.EnvironmentNativeIdentity{Verifiers: []string{"example/api"}}}
	for _, runtime := range []string{resources.RuntimeContextContainer, resources.RuntimeContextFree} {
		runtimeContext = runtime
		require.ErrorContains(t, attachLauncher(t.Context(), nil, nil, env, ""), "native-identity requires")
	}
}
