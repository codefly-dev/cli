package ci

import (
	"testing"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestCIBuildPathsExposeCachePolicy(t *testing.T) {
	for _, cmd := range []*cobra.Command{BuildCmd, RunCmd} {
		for _, name := range []string{"cache-from", "cache-to", "cache-scope", "cache-mode", "cache-backend"} {
			require.NotNil(t, cmd.Flags().Lookup(name), "%s missing %s", cmd.Name(), name)
		}
	}
}

func TestBuildCommandsForwardCacheFlags(t *testing.T) {
	// Reset rather than save-and-restore: BuildCacheFlags embeds a proto
	// message by value, so copying it copies a mutex — `go vet`'s copylocks
	// diagnostic. Nothing sets these flags before a test parses them, so the
	// pre-test state is the zero value, and cobra's flag pointers address the
	// variable's fields rather than the value they held.
	t.Cleanup(func() { buildCacheFlags = common.BuildCacheFlags{} })
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
