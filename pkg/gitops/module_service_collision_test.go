package gitops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// collidingWorkspace composes two modules that each ship a service called
// "store": the host module's (its own database) and the documents module's (its
// own). They share a service name because they run the same agent, not because
// they are one instance — they are two services, in two namespaces, with two
// databases.
func collidingWorkspace() (*resources.Workspace, *resources.Module, *resources.Module, *resources.Service) {
	workspace := &resources.Workspace{
		Name:    "acme",
		Layout:  resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{{Name: "saas"}, {Name: "documents"}},
	}
	return workspace, &resources.Module{Name: "saas"}, &resources.Module{Name: "documents"}, &resources.Service{Name: "store"}
}

// collidingEnvironment is the two-module staging environment: each module takes
// its own namespace, "platform-<module>", and one secret declaration resolves a
// different remote key per module.
func collidingEnvironment() *environments.Environment {
	return &environments.Environment{
		Name:      "staging",
		Namespace: "platform",
		ServiceSecrets: &environments.EnvironmentServiceSecrets{
			SecretStore: environments.EnvironmentSecretStoreReference{Name: "cell-secrets", Kind: "ClusterSecretStore"},
			Defaults:    &environments.EnvironmentSecretRemoteRef{Key: "{workspace}-{module}-{service}", Property: "{key}"},
		},
	}
}

// A module render drives the whole dependency graph, so one render stages the
// services of other modules too. The staged destination must therefore be keyed
// by the identity a service actually has — workspace, module and service name
// together — never by service name alone.
//
// Before the fix renderModuleTree keyed it by service name alone
// (filepath.Join(stage, "services", service.Name)), discarding the module the
// callback was handed, so the host's "store" and the documents module's "store"
// resolved to one directory.
func TestModuleStageDestinationsSeparateSameNamedServicesAcrossModules(t *testing.T) {
	owned := filepath.Join(t.TempDir(), "tree")
	require.NoError(t, os.MkdirAll(owned, 0o755))
	workspace, host, other, store := collidingWorkspace()

	destinations := moduleStageDestinations(workspace, host, owned)
	own := destinations(host, store)
	foreign := destinations(other, store)

	// The module under render keeps the committed layout its inventory pins,
	// so the published deployments/ tree is byte-identical to before.
	require.Equal(t, filepath.Join(owned, serviceUnitDir, store.Name), own,
		"the module under render must keep the committed services/<name> path")

	require.NotEqual(t, own, foreign,
		"two modules shipping a same-named service must not stage to one directory")
	require.False(t, strings.HasPrefix(foreign, owned+string(filepath.Separator)),
		"another module's service must stage outside the owned tree, never into it: %s", foreign)

	// The key is the full three-part identity, so the module segment separates
	// two modules and the workspace segment is carried too.
	require.Contains(t, foreign, filepath.Join(workspace.Name, other.Name, serviceUnitDir, store.Name))
}

// Carried through to the consequence the collision actually had: with both
// modules' stores staged, the module under render projects its own namespace
// onto its own workload, and the other module's tree is left alone.
//
// Before the fix both stores staged to one directory, so whichever the flow
// rendered last won on disk — the projection loop runs only after every flow
// has completed — and the module's own projection then found the other
// module's workload under its own unit path. validateProjectedSecretNamespace
// caught it one step later with `workload "store" references projected secret
// "secret-store" from a different namespace`. That check is correct and is left
// untouched; this test removes the corruption it was reporting.
func TestModuleRenderKeepsEachModulesStoreInItsOwnNamespace(t *testing.T) {
	owned := filepath.Join(t.TempDir(), "tree")
	require.NoError(t, os.MkdirAll(owned, 0o755))
	workspace, host, other, store := collidingWorkspace()
	env := collidingEnvironment()

	destinations := moduleStageDestinations(workspace, host, owned)
	own := destinations(host, store)
	foreign := destinations(other, store)

	// Stage in the order that used to corrupt the tree: the module under
	// render first, the other module's same-named service last.
	writeServiceTreeReferencingKeys(t, own, env.Name, env.ModuleNamespace(workspace, host.Name), store.Name, []string{"PASSWORD"})
	writeServiceTreeReferencingKeys(t, foreign, env.Name, env.ModuleNamespace(workspace, other.Name), store.Name, []string{"PASSWORD"})

	require.NoError(t, projectServiceConfiguration(t.Context(), own, store, env, moduleScope(env, workspace, host.Name)))

	// The module under render projected its own namespace and its own remote key.
	projected := readExternalSecret(t, filepath.Join(own, "overlays", env.Name, "external-secret.yaml"))
	require.Equal(t, "platform-saas", projected.Metadata.Namespace)
	require.Len(t, projected.Spec.Data, 1)
	require.Equal(t, "acme-saas-store", projected.Spec.Data[0].RemoteRef.Key)

	// The other module's store still stands, in its own namespace, untouched by
	// a render that does not own it.
	workload, err := os.ReadFile(filepath.Join(foreign, "base", "deployment.yaml"))
	require.NoError(t, err)
	require.Contains(t, string(workload), "namespace: platform-documents")
	require.NoFileExists(t, filepath.Join(foreign, "overlays", env.Name, "external-secret.yaml"),
		"a module render must not project into another module's tree")
}

func readExternalSecret(t *testing.T, path string) externalSecret {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var projected externalSecret
	require.NoError(t, yaml.Unmarshal(data, &projected))
	return projected
}
