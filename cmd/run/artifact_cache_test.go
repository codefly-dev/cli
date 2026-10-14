package run

import (
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/environments"
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
