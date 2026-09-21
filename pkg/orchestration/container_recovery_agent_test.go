package orchestration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/internal/protocoltest"
	"github.com/codefly-dev/core/agents/contract"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/runners/recoveryscope"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
)

func TestContainerRecoveryRejectsUndeclaredPeerBeforeLifecycle(t *testing.T) {
	selection := protocoltest.Install(t, "undeclared-peer")[0]
	t.Setenv(manager.AgentSourceEnv, "local")
	t.Setenv("CODEFLY_TEST_PEER_CONTRACT", "missing")
	t.Setenv(recoveryscope.EnvironmentVariable, "")
	scope, err := dockerrun.NewContainerRecoveryScope(resources.CodeflyHomeDir(), t.TempDir(), "mixed-agent-test")
	require.NoError(t, err)
	require.NoError(t, dockerrun.SetContainerRecoveryScope(scope))

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	agent, err := resources.ParseAgent(ctx, resources.ServiceAgent, selection)
	require.NoError(t, err)
	const cacheKey = "container-recovery-legacy-agent"
	t.Cleanup(func() { services.ClearAgent(cacheKey) })
	client, err := services.LoadAgent(ctx, agent, cacheKey)
	require.ErrorIs(t, err, contract.ErrIncompatible)
	require.ErrorContains(t, err, "does not declare a CLI-agent protocol version")
	require.Nil(t, client)

	calls := protocoltest.Calls(t, os.Getenv("CODEFLY_TEST_PEER_ROOT"))
	require.NotEmpty(t, calls)
	for _, call := range calls {
		require.Equal(t, "Agent.GetAgentInformation", call.Method, "rejected peers must not receive lifecycle calls")
	}
}
