package run

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/nativecluster"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// The artifact-cache group a native run hands a service that declares it
// carries one value: ROOT, the absolute path of the workspace's artifact
// cache, where the host's native adapter reads a bundle by its digest.
func TestArtifactCacheGroupNamesTheWorkspaceCacheRoot(t *testing.T) {
	workspace := t.TempDir()
	group, err := artifactCacheGroup(workspace)
	require.NoError(t, err)
	require.Len(t, group.Infos, 1)
	require.Equal(t, environments.ArtifactCacheConfiguration, group.Infos[0].Name)
	require.Len(t, group.Infos[0].ConfigurationValues, 1)
	value := group.Infos[0].ConfigurationValues[0]
	require.Equal(t, environments.ArtifactCacheRootKey, value.Key)
	require.True(t, filepath.IsAbs(value.Value))
	require.Equal(t, composition.ArtifactCacheDir(workspace), value.Value)
	require.False(t, value.Secret, "a path to software bytes is no credential")
}

// The group's value names a directory that exists: the host opens it
// read-only when it starts, before the first bundle is retained there.
func TestArtifactCacheGroupCreatesTheCacheRoot(t *testing.T) {
	workspace := t.TempDir()
	group, err := artifactCacheGroup(workspace)
	require.NoError(t, err)
	info, err := os.Stat(group.Infos[0].ConfigurationValues[0].Value)
	require.NoError(t, err, "the root exists once the value is handed out")
	require.True(t, info.IsDir())
}

// A component that declares a page and built no bundle is refused by name,
// with what to build; a headless component needs none, and a leftover dist
// is not its page.
func TestANativeRunRefusesADeclaredPageWithoutItsBuiltBundle(t *testing.T) {
	ctx := context.Background()
	module := &resources.Module{Name: "wiki", ServiceEntry: "api"}
	module.WithDir(t.TempDir())
	entry := filepath.Join(module.ServicesDir(), "api")
	require.NoError(t, os.MkdirAll(entry, 0o755))
	cluster := &nativeCluster{workspace: &resources.Workspace{Name: "acme"}}

	bundle, err := cluster.frontendBundle(ctx, module)
	require.NoError(t, err, "headless: no page, no bundle needed")
	require.Nil(t, bundle)

	require.NoError(t, os.WriteFile(filepath.Join(entry, solutionrun.FrontendFileName), []byte("title: Wiki\norder: 1\nexposed_module: ./Page\nhost_contract: 1\n"), 0o600))
	_, err = cluster.frontendBundle(ctx, module)
	require.ErrorContains(t, err, "its bundle is not built")
	require.ErrorContains(t, err, filepath.Join(module.ServicesDir(), "fe-remote"), "the source to build is named")
	require.ErrorContains(t, err, solutionrun.FrontendBundleManifest)
}

// Forgetting a withdrawn workload drops what this run kept for it, and only
// that: the identity, the projection, the documents, the withheld mark and the
// delivered record a restart would replay; another workload's stays.
func TestForgetDropsOnlyTheWithdrawnWorkload(t *testing.T) {
	cluster := &nativeCluster{
		stateDir:    t.TempDir(),
		identities:  map[string]gitops.NativeIdentity{"wiki/api": {Module: "wiki"}, "crm/api": {Module: "crm"}},
		projections: map[string]nativecluster.Projection{"wiki/api": {}, "crm/api": {}},
		documents:   map[string]*gitops.NativeDocuments{"wiki/api": {}, "crm/api": {}},
		withheld:    map[string]bool{"wiki/api": true},
	}
	require.NoError(t, cluster.persistDelivered("wiki/api", &gitops.NativeDocuments{}))
	require.NoError(t, cluster.persistDelivered("crm/api", &gitops.NativeDocuments{}))
	cluster.forget("wiki/api")
	_, known := cluster.identity("wiki/api")
	require.False(t, known)
	require.NotContains(t, cluster.projections, "wiki/api")
	require.NotContains(t, cluster.documents, "wiki/api")
	require.NotContains(t, cluster.withheld, "wiki/api")
	require.NoFileExists(t, filepath.Join(cluster.deliveredDir(), deliveredFileName("wiki/api")))
	_, kept := cluster.identity("crm/api")
	require.True(t, kept)
	require.Contains(t, cluster.documents, "crm/api")
	require.FileExists(t, filepath.Join(cluster.deliveredDir(), deliveredFileName("crm/api")))
}

// The delivery host that declares the federation group receives, from the
// host block, the three keys that turn its delivery surface on: the
// coordinate, the ownership domain the run's policy admits the signer for,
// and the pinned-keys trust policy. Another service, or a host block without
// a coordinate and a domain, receives nothing of it.
func TestTheDeliveryHostReceivesItsFederationSurfaceFromTheHostBlock(t *testing.T) {
	ctx := context.Background()
	host := &environments.EnvironmentHost{Coordinate: "obin/local/dev", Component: "saas", Domain: "obin", Audience: "accounts", TrustDomain: "cluster.local", Delivery: "saas/accounts/rest"}
	cluster := &nativeCluster{env: &environments.Environment{Name: "local", Host: host}, hostModule: "saas", hostService: "accounts", identities: map[string]gitops.NativeIdentity{}}
	accounts := &resources.Service{Name: "accounts", WorkspaceConfigurationDependencies: []string{"abuse-protection", environments.FederationConfiguration}}
	accounts.WithModule("saas")
	groups, err := cluster.renderOwnedGroups(ctx, accounts)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, environments.FederationConfiguration, groups[0].Infos[0].Name)
	values := map[string]string{}
	for _, value := range groups[0].Infos[0].ConfigurationValues {
		values[value.Key] = value.Value
		require.False(t, value.Secret)
	}
	require.Equal(t, map[string]string{
		environments.FederationHostCoordinateKey:       "obin/local/dev",
		environments.FederationHostOwnershipDomainsKey: "obin",
		environments.FederationHostTrustPolicyKey:      environments.FederationHostTrustPolicyKeys,
	}, values)

	gateway := &resources.Service{Name: "auth-gateway", WorkspaceConfigurationDependencies: []string{environments.FederationConfiguration}}
	gateway.WithModule("saas")
	groups, err = cluster.renderOwnedGroups(ctx, gateway)
	require.NoError(t, err)
	require.Empty(t, groups, "only the delivery host answers for the coordinate")

	silent := &resources.Service{Name: "accounts", WorkspaceConfigurationDependencies: []string{"abuse-protection"}}
	silent.WithModule("saas")
	groups, err = cluster.renderOwnedGroups(ctx, silent)
	require.NoError(t, err)
	require.Empty(t, groups, "a host that does not declare federation is handed none of it")

	_, err = federationGroup(&environments.EnvironmentHost{Coordinate: "obin/local/dev"})
	require.ErrorContains(t, err, "host.domain")
}
