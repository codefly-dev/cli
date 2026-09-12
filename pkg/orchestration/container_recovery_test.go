package orchestration

import (
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
)

func TestContainerRecoveryRequiresAgentAcknowledgementBeforeDockerInit(t *testing.T) {
	t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, "")
	scope, err := dockerrun.NewContainerRecoveryScope(t.TempDir(), t.TempDir(), "test")
	require.NoError(t, err)
	require.NoError(t, dockerrun.SetContainerRecoveryScope(scope))
	for _, tc := range []struct {
		name, runtime, ack string
		reject             bool
	}{
		{"legacy Docker agent", resources.RuntimeContextContainer, "", true},
		{"legacy free agent", resources.RuntimeContextFree, "", true},
		{"wrong scope", resources.RuntimeContextContainer, "foreign", true},
		{"acknowledged", resources.RuntimeContextContainer, dockerrun.InheritedContainerRecoveryScope(), false},
		{"native", resources.RuntimeContextNative, "", false},
		{"nix", resources.RuntimeContextNix, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &Runner{runtimeContext: tc.runtime, instance: &services.Instance{Identity: &resources.ServiceIdentity{Name: "db", Module: "infra"}, ContainerRecoveryScope: tc.ack}}
			if tc.reject {
				// No Runtime client or World is installed: the real Init must
				// return before configuration, port allocation or any agent RPC.
				_, err := runner.Init(t.Context())
				require.ErrorContains(t, err, "did not acknowledge this run's container recovery scope")
			} else {
				require.NoError(t, runner.validateContainerRecovery())
			}
		})
	}
	t.Run("invalid inherited identity", func(t *testing.T) {
		t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, "malformed")
		runner := &Runner{runtimeContext: resources.RuntimeContextContainer, instance: &services.Instance{Identity: &resources.ServiceIdentity{Name: "db", Module: "infra"}}}
		_, err := runner.Init(t.Context())
		require.ErrorContains(t, err, "invalid container recovery identity")
	})
}
