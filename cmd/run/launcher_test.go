package run

import (
	"context"
	"github.com/codefly-dev/core/workloadidentity"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/run/identity"
	"github.com/codefly-dev/core/resources"
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
	t.Run("the run vouches for its carriers, and nothing else", func(t *testing.T) {
		token, err := vouch.Issue(vouch.Workload("model", workloadidentity.DeliveryService))
		require.NoError(t, err)
		_, err = attester.Attest(ctx, token, env.Host.Audience)
		require.NoError(t, err)
		_, err = vouch.Issue(vouch.Workload("elsewhere", "api"))
		require.ErrorContains(t, err, "did not declare elsewhere/api")
		_, err = vouch.Issue(vouch.Workload(workloadidentity.AppliedObserver.Module, workloadidentity.AppliedObserver.Service))
		require.Error(t, err, "nothing observes applied state over a route any more")
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
