package ci

import (
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCIBuildPathsExposeCachePolicy(t *testing.T) {
	for _, cmd := range []*cobra.Command{BuildCmd, RunCmd} {
		for _, name := range []string{"cache-from", "cache-to", "cache-scope", "cache-mode", "cache-backend"} {
			require.NotNil(t, cmd.Flags().Lookup(name), "%s missing %s", cmd.Name(), name)
		}
	}
}

func TestBuildCommandsForwardCacheFlags(t *testing.T) {
	previous := buildCacheFlags
	t.Cleanup(func() { buildCacheFlags = previous })
	for _, cmd := range []*cobra.Command{BuildCmd, RunCmd} {
		require.NoError(t, cmd.ParseFlags([]string{"--cache-from", "ghcr.io/org/cache", "--cache-to", "ghcr.io/org/cache", "--cache-scope", "app/api", "--cache-mode", "min"}))
		cache, err := buildCacheFlags.Policy()
		require.NoError(t, err)
		require.Equal(t, []string{"ghcr.io/org/cache"}, cache.Imports)
		require.Equal(t, []string{"ghcr.io/org/cache"}, cache.Exports)
		require.Equal(t, "app/api", cache.Scope)
		require.Equal(t, "min", cache.Mode)
	}
}
