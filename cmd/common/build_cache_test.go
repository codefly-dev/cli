package common

import (
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestBuildCacheFlagsValidateAndPreserveReadOnlyPolicy(t *testing.T) {
	for _, tt := range []struct {
		name  string
		args  []string
		valid bool
	}{
		{"disabled", nil, true},
		{"read-only", []string{"--cache-from", "ghcr.io/org/cache", "--cache-scope", "workspace/protected"}, true},
		{"write", []string{"--cache-to", "ghcr.io/org/cache", "--cache-scope", "workspace/protected"}, true},
		{"missing scope", []string{"--cache-from", "ghcr.io/org/cache"}, false},
		{"unsupported", []string{"--cache-backend", "gha"}, false},
		{"injected ref", []string{"--cache-to", "ghcr.io/org/cache,mode=max", "--cache-scope", "x"}, false},
		{"empty transport", []string{"--cache-scope", "x"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			var flags BuildCacheFlags
			flags.Bind(cmd)
			require.NoError(t, cmd.ParseFlags(tt.args))
			policy, err := flags.Policy()
			if !tt.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tt.name == "disabled" {
				require.Nil(t, policy)
				return
			}
			if tt.name == "read-only" {
				require.Empty(t, policy.Exports)
			}
			policy.Scope = "modified"
			fresh, err := flags.Policy()
			require.NoError(t, err)
			require.Equal(t, "workspace/protected", fresh.Scope)
		})
	}
}
