package orchestration

import (
	"context"
	"debug/buildinfo"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/runners/recoveryscope"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
)

// The reader of the process marker is the agent binary's Core, not the CLI's.
// Exercise a published agent in a private cache so this regression cannot pass
// merely because the test and its in-process helper share the same parser.
func TestContainerRecoveryRejectsAReleasedLegacyAgent(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	t.Setenv(manager.AgentSourceEnv, "github")
	t.Setenv(recoveryscope.EnvironmentVariable, "")
	scope, err := dockerrun.NewContainerRecoveryScope(resources.CodeflyHomeDir(), t.TempDir(), "mixed-agent-test")
	require.NoError(t, err)
	require.NoError(t, dockerrun.SetContainerRecoveryScope(scope))

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	agent, err := resources.ParseAgent(ctx, resources.ServiceAgent, "go:0.0.47")
	require.NoError(t, err)
	const cacheKey = "container-recovery-legacy-agent"
	t.Cleanup(func() { services.ClearAgent(cacheKey) })
	client, err := services.LoadAgent(ctx, agent, cacheKey)
	require.ErrorContains(t, err, "does not declare a CLI-agent protocol version")
	require.Nil(t, client)

	path, err := agent.Path(ctx)
	require.NoError(t, err)
	build, err := buildinfo.ReadFile(path)
	require.NoError(t, err)
	var coreVersion string
	for _, dep := range build.Deps {
		if dep.Path == "github.com/codefly-dev/core" {
			coreVersion = dep.Version
		}
	}
	require.Equal(t, "v0.3.27", coreVersion, "the published fixture must retain its pre-v2 Core")
}
