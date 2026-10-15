package run

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/nativecluster"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// writeNonGoWorkload lays down a workspace whose one module runs its service
// on an agent that is not the Go family, and returns the loaded service.
func writeNonGoWorkload(t *testing.T) (*resources.Workspace, *resources.Service) {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		resources.WorkspaceConfigurationName:                                 "name: acme\nlayout: modules\nmodules:\n  - name: model\n",
		filepath.Join("modules", "model", resources.ModuleConfigurationName): "kind: module\nname: model\nservices:\n  - name: model\n",
		filepath.Join("modules", "model", "services", "model", resources.ServiceConfigurationName): `kind: service
name: model
version: 0.0.0
module-identity: true
agent:
  kind: runtime::service
  name: python-fastapi
  version: 0.0.112
  publisher: codefly.dev
`,
	}
	for relative, content := range files {
		full := filepath.Join(root, relative)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	service, err := workspace.LoadService(ctx, &resources.ServiceWithModule{Name: "model", Module: "model"})
	require.NoError(t, err)
	return workspace, service
}

// A delivery reads the build from the agent's report, so a workload whose
// agent has not adopted the contract is refused by name — and the refusal
// arrives before the run touches its authority, its gateway or the host's
// envelope. This is the shape of #989: the old path joined
// <service>/.cache/native/main for every agent and failed on a missing file
// that a python service was never going to have.
func TestANativeDeliveryRefusesAWorkloadWhoseAgentReportsNoArtifact(t *testing.T) {
	workspace, service := writeNonGoWorkload(t)
	unique := resources.WithUnique(service).Unique()
	cluster := &nativeCluster{
		workspace: workspace,
		workloads: map[string]nativecluster.Workload{unique: {Namespace: "acme-model", Account: "model"}},
		// A flow holding no manager for the service answers as an agent that
		// reported nothing: there is no started artifact to be had.
		flow: &orchestration.Flow{},
	}
	identity := gitops.NativeIdentity{Module: "model", Service: "model", Account: "model"}

	err := cluster.deliver(context.Background(), unique, identity, service)
	require.ErrorContains(t, err, "codefly.dev/python-fastapi:0.0.112", "the refusal must name the agent that has to adopt the contract")
	require.ErrorContains(t, err, "runtime-started-artifact/v1")
	require.ErrorContains(t, err, "model/model")
	require.NotContains(t, err.Error(), ".cache", "the refusal must not name a path from one agent family's layout")
	require.NotContains(t, err.Error(), "no such file", "a missing file is not the diagnosis: the agent reported nothing")

	// Refused before anything is claimed on the workload's behalf: no
	// documents, no approved build, nothing for the host to reconcile.
	require.Empty(t, cluster.documents)
	require.Empty(t, cluster.builds)
}

// A workload the run never projected an identity for is a different failure
// from one whose agent reported nothing, and must read as one.
func TestANativeDeliveryNamesAnUnprojectedWorkloadAsSuch(t *testing.T) {
	workspace, service := writeNonGoWorkload(t)
	cluster := &nativeCluster{workspace: workspace, workloads: map[string]nativecluster.Workload{}}
	err := cluster.deliver(context.Background(), resources.WithUnique(service).Unique(),
		gitops.NativeIdentity{Module: "model", Service: "model"}, service)
	require.ErrorContains(t, err, "no projected identity")
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
      envelope_revision: 1
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
	cluster := &nativeCluster{workspace: workspace, env: env, identities: identities, hostIdentityAnchor: "/run/host/identity-anchor.json"}
	before := cluster.hostIdentityAnchor

	reloaded := composeModule(t, workspace, "wiki")
	added, err := cluster.admit(ctx, reloaded)
	require.NoError(t, err)
	require.Equal(t, []string{"wiki/api"}, added)
	identity, ok := cluster.identity("wiki/api")
	require.True(t, ok)
	require.Equal(t, gitops.NativeIdentity{Module: "wiki", Service: "api", Account: "api", SPIFFEID: env.Host.SPIFFEID("acme-wiki", "api")}, identity)
	require.Same(t, reloaded, cluster.composed(), "the run composes the reloaded workspace from now on")
	_, stillKnown := cluster.identity("model/model")
	require.True(t, stillKnown)
	require.Nil(t, cluster.authority, "admission starts no authority")
	require.Equal(t, before, cluster.hostIdentityAnchor, "the host's identity anchor is untouched")

	again, err := cluster.admit(ctx, reloaded)
	require.NoError(t, err)
	require.Empty(t, again, "admitting the same composition twice adds nothing")

	t.Run("a known principal is never replaced", func(t *testing.T) {
		changed := &nativeCluster{workspace: workspace, env: env, identities: map[string]gitops.NativeIdentity{
			"model/model": {Module: "model", Service: "model", Account: "model", SPIFFEID: "spiffe://cluster.local/ns/elsewhere/sa/model"},
		}}
		_, err := changed.admit(ctx, reloaded)
		require.ErrorContains(t, err, "model/model runs as spiffe://cluster.local/ns/elsewhere/sa/model")
		require.ErrorContains(t, err, "never replaced")
		_, admitted := changed.identity("wiki/api")
		require.False(t, admitted, "a refused admission leaves the run as it was")
		require.Same(t, workspace, changed.composed())
	})
	t.Run("a module whose namespace the composition rule moves is a replacement, refused", func(t *testing.T) {
		// A workspace composing one module gives it the environment's
		// namespace; composing a second moves the first to its own, and
		// with it its mesh principal. That is not an addition the run can
		// make under a running host: it is refused by name.
		root := writeNonGoWorkloadDir(t, "solo")
		single, err := resources.LoadWorkspaceFromDir(ctx, root)
		require.NoError(t, err)
		soloEnv, err := environments.Select(single, "local")
		require.NoError(t, err)
		soloIdentities, err := gitops.ResolveNativeIdentities(ctx, single, soloEnv)
		require.NoError(t, err)
		solo := &nativeCluster{workspace: single, env: soloEnv, identities: soloIdentities}
		_, err = solo.admit(ctx, composeModule(t, single, "second"))
		require.ErrorContains(t, err, "solo/api runs as "+soloEnv.Host.SPIFFEID("acme", "api"))
		require.ErrorContains(t, err, "never replaced")
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

// writeNonGoWorkloadDir is writeNonGoWorkload's layout under a module of the
// given name, in a hosted environment, returning the directory.
func writeNonGoWorkloadDir(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		resources.WorkspaceConfigurationName:                                                  "name: acme\nlayout: modules\nmodules:\n  - name: " + name + "\nenvironments:\n  - name: local\n    namespace: acme\n    host:\n      coordinate: example/local/dev\n      component: host\n      domain: acme\n      audience: accounts\n      trust_domain: cluster.local\n      envelope_revision: 1\n      delivery: " + name + "/api/rest\n",
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
