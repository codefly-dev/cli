package run

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// pageModule is a component whose entry service declares a page, with its npm
// project (package.json, package-lock.json, one source) beside the services.
func pageModule(t *testing.T) (*resources.Module, string) {
	t.Helper()
	module := &resources.Module{Name: "wiki", ServiceEntry: "api"}
	module.WithDir(t.TempDir())
	entry := filepath.Join(module.ServicesDir(), "api")
	require.NoError(t, os.MkdirAll(entry, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(entry, solutionrun.FrontendFileName), []byte("title: Wiki\norder: 1\nexposed_module: ./Page\nhost_contract: 1\n"), 0o600))
	remote := filepath.Join(module.ServicesDir(), "fe-remote")
	require.NoError(t, os.MkdirAll(remote, 0o755))
	for name, content := range map[string]string{"package.json": `{"scripts":{"build":"owner-build"}}`, "package-lock.json": `{"lockfileVersion":3}`, "Page.tsx": "page v1"} {
		require.NoError(t, os.WriteFile(filepath.Join(remote, name), []byte(content), 0o600))
	}
	return module, remote
}

// fakeTool puts an executable named name, running body, first on PATH.
func fakeTool(t *testing.T, bin, name, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nset -eu\n"+body), 0o700))
}

// A declared page is built natively with the frontend's own npm build, once per
// change of its own inputs: an unchanged frontend, or an edit elsewhere in the
// component (a Go service hot-reloading), reuses the retained bundle; a source
// edit rebuilds; a lockfile edit installs again first. A failed build names its
// command and carries its output, and never delivers an old bundle.
func TestANativeRunBuildsItsDeclaredPageOncePerChangeOfItsInputs(t *testing.T) {
	ctx := context.Background()
	module, remote := pageModule(t)
	workspace := &resources.Workspace{Name: "acme"}
	workspace.WithDir(t.TempDir())
	cluster := &launcher{workspace: workspace, stateDir: t.TempDir(), flow: &fakeLauncherFlow{}}

	headless := &resources.Module{Name: "headless", ServiceEntry: "api"}
	headless.WithDir(t.TempDir())
	bundle, err := cluster.frontendBundle(ctx, headless)
	require.NoError(t, err, "headless: no build needed")
	require.Nil(t, bundle)

	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("ASSET_PREFIX", "http://leaked.example")
	fakeTool(t, bin, "npm", `echo "$*" >> `+calls+`
[ -z "${ASSET_PREFIX:-}" ] || { echo 'ASSET_PREFIX reached the build' >&2; exit 3; }
case "$*" in
ci) mkdir -p node_modules ;;
"run build") mkdir -p dist && printf '{"page":"%s"}' "$(cat Page.tsx)" > dist/mf-manifest.json ;;
esac
`)
	build := func() string {
		t.Helper()
		bundle, err := cluster.frontendBundle(ctx, module)
		require.NoError(t, err)
		archive := filepath.Join(composition.ArtifactCacheDir(workspace.Dir()), strings.TrimPrefix(string(bundle.Digest), "sha256:"))
		contents, err := os.ReadFile(archive)
		require.NoError(t, err)
		return string(contents)
	}
	invocations := func() []string {
		data, _ := os.ReadFile(calls)
		_ = os.Remove(calls)
		return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(data)), "run build", "build"))
	}

	require.Contains(t, build(), "page v1")
	require.Equal(t, []string{"ci", "build"}, invocations())
	require.Contains(t, build(), "page v1")
	require.Empty(t, invocations(), "unchanged inputs reuse the retained bundle")
	require.NoError(t, os.WriteFile(filepath.Join(module.ServicesDir(), "api", "main.go"), []byte("package main"), 0o600))
	require.Contains(t, build(), "page v1")
	require.Empty(t, invocations(), "an edit outside the frontend builds nothing")
	require.NoError(t, os.WriteFile(filepath.Join(remote, "Page.tsx"), []byte("page v2"), 0o600))
	require.Contains(t, build(), "page v2")
	require.Equal(t, []string{"build"}, invocations(), "a source edit rebuilds without reinstalling")
	require.NoError(t, os.WriteFile(filepath.Join(remote, "package-lock.json"), []byte(`{"lockfileVersion":3,"packages":{}}`), 0o600))
	build()
	require.Equal(t, []string{"ci", "build"}, invocations(), "a lockfile edit installs again")

	require.NoError(t, os.WriteFile(filepath.Join(remote, "Page.tsx"), []byte("page v3"), 0o600))
	fakeTool(t, bin, "npm", "echo 'owner bundler: Page.tsx failed' >&2\nexit 7\n")
	bundle, err = cluster.frontendBundle(ctx, module)
	require.Nil(t, bundle, "a failed build never delivers the old bundle")
	require.ErrorContains(t, err, "build the frontend of wiki (npm run build in")
	require.ErrorContains(t, err, "owner bundler: Page.tsx failed")

	require.NoError(t, os.Symlink("Page.tsx", filepath.Join(remote, "Alias.tsx")))
	_, err = cluster.frontendBundle(ctx, module)
	require.ErrorContains(t, err, "Alias.tsx is not a regular file")
}

// A page whose owning service runs in a container is built in the pinned
// builder image, as deployment builds it; the controlled Docker peer exercises
// the process boundary without a registry or toolchain.
func TestAContainerizedPageBuildsInTheBuilderImage(t *testing.T) {
	ctx := context.Background()
	module, _ := pageModule(t)
	workspace := &resources.Workspace{Name: "acme"}
	workspace.WithDir(t.TempDir())
	cluster := &launcher{workspace: workspace, stateDir: t.TempDir(), flow: &fakeLauncherFlow{runtime: resources.RuntimeContextContainer}}
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	fakeTool(t, bin, "npm", "echo 'npm must not run on the host' >&2\nexit 9\n")
	fakeTool(t, bin, "docker", `recipe=$(cat)
case "$recipe" in *'npm ci && npm run build'*) ;; *) echo 'missing owner build' >&2; exit 2;; esac
for arg in "$@"; do
 case "$arg" in type=local,dest=*) destination=${arg#type=local,dest=};; esac
done
mkdir -p "$destination"
echo '{"name":"built-wiki"}' > "$destination/mf-manifest.json"
`)
	bundle, err := cluster.frontendBundle(ctx, module)
	require.NoError(t, err)
	contents, err := os.ReadFile(filepath.Join(composition.ArtifactCacheDir(workspace.Dir()), strings.TrimPrefix(string(bundle.Digest), "sha256:")))
	require.NoError(t, err)
	require.Contains(t, string(contents), "built-wiki")

	fakeTool(t, bin, "docker", "cat >/dev/null\necho 'owner build returned without artifacts'\n")
	require.NoError(t, os.WriteFile(filepath.Join(module.ServicesDir(), "fe-remote", "Page.tsx"), []byte("page v2"), 0o600))
	_, err = cluster.frontendBundle(ctx, module)
	require.ErrorContains(t, err, "frontend build of wiki wrote no regular mf-manifest.json")
	require.ErrorContains(t, err, "owner build returned without artifacts")
}

// The frontend is built before the delivery lock is taken: a module whose
// build fails says so while another delivery holds the lock.
func TestAFrontendBuildHoldsNoDeliveryLock(t *testing.T) {
	root := writeNonGoWorkloadDir(t, "model")
	moduleFile := filepath.Join(root, "modules", "model", resources.ModuleConfigurationName)
	require.NoError(t, os.WriteFile(moduleFile, []byte("kind: module\nname: model\nservice-entry: api\nservices:\n  - name: api\n"), 0o644))
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(context.Background(), "model")
	require.NoError(t, err)
	require.NotEmpty(t, module.ServiceEntry)
	entry := filepath.Join(module.ServicesDir(), module.ServiceEntry)
	require.NoError(t, os.MkdirAll(entry, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(entry, solutionrun.FrontendFileName), []byte("title: Model\norder: 1\nexposed_module: ./Page\nhost_contract: 1\n"), 0o600))
	cluster, _, unique := launcherWithFakeFlow(t, func(string) bool { return true })
	cluster.workspace, cluster.stateDir = workspace, t.TempDir()
	cluster.running[unique] = true
	require.NoError(t, cluster.acquireDelivery(context.Background()))
	defer cluster.delivery.Release(1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.ErrorContains(t, cluster.deliver(ctx, "model"), "holds no npm project", "the build ran and failed without waiting for the lock")
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
