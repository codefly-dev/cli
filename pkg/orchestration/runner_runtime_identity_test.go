package orchestration

import (
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestRunnerRuntimeOverridesCarryAuthoritativeNamingScope(t *testing.T) {
	input := map[string]string{
		"APPLICATION_MODE":          "dogfood",
		resources.NamingScopePrefix: "caller-must-not-override-runtime-identity",
	}
	runner := &Runner{
		world:     &World{Env: &resources.Environment{NamingScope: "stable"}},
		overrides: input,
	}

	got := runner.runtimeOverrides()

	require.Equal(t, "dogfood", got["APPLICATION_MODE"])
	require.Equal(t, "stable", got[resources.NamingScopePrefix])
	require.Equal(
		t,
		"caller-must-not-override-runtime-identity",
		input[resources.NamingScopePrefix],
		"deriving runtime identity must not mutate caller-owned overrides",
	)
}

func TestRunnerRuntimeOverridesOmitUnselectedNamingScope(t *testing.T) {
	runner := &Runner{world: &World{Env: &resources.Environment{}}}
	require.Empty(t, runner.runtimeOverrides())
}
