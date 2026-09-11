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
