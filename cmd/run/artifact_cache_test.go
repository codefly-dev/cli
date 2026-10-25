package run

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/run/identity"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
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

// A declared frontend is built before delivery, even with a leftover dist.
// The controlled Docker peer exercises the CLI process boundary without a
// released agent, npm registry or provider toolchain in this gate.
func TestANativeRunBuildsItsDeclaredPageBeforeRetainingIt(t *testing.T) {
	ctx := context.Background()
	module := &resources.Module{Name: "wiki", ServiceEntry: "api"}
	module.WithDir(t.TempDir())
	entry := filepath.Join(module.ServicesDir(), "api")
	require.NoError(t, os.MkdirAll(entry, 0o755))
	workspace := &resources.Workspace{Name: "acme"}
	workspace.WithDir(t.TempDir())
	cluster := &launcher{workspace: workspace}

	bundle, err := cluster.frontendBundle(ctx, module)
	require.NoError(t, err, "headless: no build needed")
	require.Nil(t, bundle)

	require.NoError(t, os.WriteFile(filepath.Join(entry, solutionrun.FrontendFileName), []byte("title: Wiki\norder: 1\nexposed_module: ./Page\nhost_contract: 1\n"), 0o600))
	remote := filepath.Join(module.ServicesDir(), "fe-remote")
	require.NoError(t, os.MkdirAll(remote, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(remote, "package.json"), []byte(`{"scripts":{"build":"owner-build"}}`), 0o600))
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	tool := filepath.Join(bin, "docker")
	writeTool := func(body string) { require.NoError(t, os.WriteFile(tool, []byte("#!/bin/sh\nset -eu\n"+body), 0o700)) }
	writeTool(`recipe=$(cat)
case "$recipe" in *'npm ci && npm run build'*) ;; *) echo 'missing owner build' >&2; exit 2;; esac
for arg in "$@"; do
 case "$arg" in type=local,dest=*) destination=${arg#type=local,dest=};; esac
 source=$arg
done
[ ! -d "$source/services/fe-remote/dist" ]
mkdir -p "$destination"
echo '{"name":"built-wiki"}' > "$destination/mf-manifest.json"
`)
	bundle, err = cluster.frontendBundle(ctx, module)
	require.NoError(t, err)
	require.NotNil(t, bundle)
	archive := filepath.Join(composition.ArtifactCacheDir(workspace.Dir()), strings.TrimPrefix(string(bundle.Digest), "sha256:"))
	contents, err := os.ReadFile(archive)
	require.NoError(t, err)
	require.Contains(t, string(contents), "built-wiki")
	require.NotContains(t, string(contents), "stale")

	require.NoError(t, os.MkdirAll(filepath.Join(remote, "dist"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(remote, "dist", solutionrun.FrontendBundleManifest), []byte("stale"), 0o600))
	writeTool("cat >/dev/null\necho 'owner bundler: Page.tsx failed' >&2\nexit 7\n")
	bundle, err = cluster.frontendBundle(ctx, module)
	require.Nil(t, bundle, "a failed build never delivers the old dist or cached payload")
	require.ErrorContains(t, err, "frontend of wiki")
	require.ErrorContains(t, err, "owner bundler: Page.tsx failed")

	writeTool("cat >/dev/null\necho 'owner build returned without artifacts'\n")
	bundle, err = cluster.frontendBundle(ctx, module)
	require.Nil(t, bundle)
	require.ErrorContains(t, err, "frontend build of wiki wrote no regular mf-manifest.json")
	require.ErrorContains(t, err, "owner build returned without artifacts")
}

// Forgetting a stopped workload drops what this run kept for it, and only
// that: the identity, the projection, the issued workload and whether it
// runs; another workload's stays, and so does its module's delivered
// manifest, which a withdrawal still needs.
func TestForgetDropsOnlyTheStoppedWorkload(t *testing.T) {
	vouch, err := identity.Start("cluster.local", "accounts", t.TempDir())
	require.NoError(t, err)
	vouch.DeclareModule("wiki", "api")
	vouch.DeclareModule("crm", "api")
	cluster := &launcher{
		vouch:      vouch,
		identities: map[string]gitops.NativeIdentity{"wiki/api": {Module: "wiki", Service: "api"}, "crm/api": {Module: "crm", Service: "api"}},
		running:    map[string]bool{"wiki/api": true, "crm/api": true},
		delivered:  map[string]*solutionhost.ModuleManifest{"wiki": {}, "crm": {}},
	}
	for _, unique := range []string{"wiki/api", "crm/api"} {
		_, err := vouch.Project(unique, vouch.Workload(cluster.identities[unique].Module, cluster.identities[unique].Service))
		require.NoError(t, err)
	}
	cluster.forget("wiki/api")
	_, known := cluster.identity("wiki/api")
	require.False(t, known)
	_, projected := vouch.Projected("wiki/api")
	require.False(t, projected)
	require.NotContains(t, cluster.running, "wiki/api")
	require.Contains(t, cluster.delivered, "wiki")
	_, kept := cluster.identity("crm/api")
	require.True(t, kept)
	_, projected = vouch.Projected("crm/api")
	require.True(t, projected)
	require.True(t, cluster.running["crm/api"])
}

// The delivery host that declares the federation group receives, from the
// host block, the three keys that turn its delivery surface on: the
// coordinate, the ownership domain the run's policy admits the signer for,
// and the pinned-keys trust policy. Another service, or a host block without
// a coordinate and a domain, receives nothing of it.
func TestTheDeliveryHostReceivesItsFederationSurfaceFromTheHostBlock(t *testing.T) {
	ctx := context.Background()
	host := &environments.EnvironmentHost{Coordinate: "obin/local/dev", Component: "saas", Domain: "obin", Audience: "accounts", TrustDomain: "cluster.local", Delivery: "saas/accounts/rest"}
	cluster := &launcher{env: &environments.Environment{Name: "local", Host: host}, hostModule: "saas", hostService: "accounts", identities: map[string]gitops.NativeIdentity{}}
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
