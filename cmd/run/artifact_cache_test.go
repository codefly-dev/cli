package run

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/environments"
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
