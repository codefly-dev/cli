package build

import (
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestBuildCommandsExposeCachePolicy(t *testing.T) {
	for _, cmd := range []*cobra.Command{ServiceCmd, ModuleCmd} {
		for _, name := range []string{"cache-from", "cache-to", "cache-scope", "cache-mode", "cache-backend"} {
			require.NotNil(t, cmd.Flags().Lookup(name), "%s missing %s", cmd.Name(), name)
		}
	}
}
