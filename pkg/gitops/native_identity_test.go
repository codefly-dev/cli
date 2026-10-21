package gitops

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/stretchr/testify/require"
)

func TestRenderRefusesNativeIdentityBeforeGenerating(t *testing.T) {
	env := &environments.Environment{Host: &environments.EnvironmentHost{}, NativeIdentity: &environments.EnvironmentNativeIdentity{Verifiers: []string{"example/api"}}}
	generated := false
	_, err := RenderOwnedTree(t.Context(), &RenderOptions{Target: env, Destination: t.TempDir()}, func(context.Context, string) error {
		generated = true
		return nil
	})
	require.ErrorContains(t, err, "native-identity requires")
	require.False(t, generated)
}
