package orchestration

import (
	"context"
	"debug/buildinfo"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/manager"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// The reader of the process marker is the agent binary's Core, not the CLI's.
// Exercise a published agent in a private cache so this regression cannot pass
// merely because the test and its in-process helper share the same parser.
func TestContainerRecoveryRejectsAReleasedLegacyAgent(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	t.Setenv(manager.AgentSourceEnv, "github")
	t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, "")
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
	require.NoError(t, err)

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

	var headers metadata.MD
	_, err = client.GetAgentInformation(ctx, &agentv0.AgentInformationRequest{}, grpc.Header(&headers))
	require.NoError(t, err)
	acknowledgement := headers.Get(dockerrun.ContainerRecoveryScopeHeader)
	require.Empty(t, acknowledgement)
	for _, runtimeContext := range []string{resources.RuntimeContextContainer, resources.RuntimeContextFree} {
		runner := &Runner{
			runtimeContext: runtimeContext,
			instance: &services.Instance{
				Identity:               &resources.ServiceIdentity{Name: "legacy", Module: "test"},
				ContainerRecoveryScope: strings.Join(acknowledgement, ""),
			},
		}
		_, err := runner.Init(ctx)
		require.ErrorContains(t, err, "did not acknowledge this run's container recovery scope")
	}
}
