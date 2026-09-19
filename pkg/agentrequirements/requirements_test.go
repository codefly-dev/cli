package agentrequirements

import (
	"testing"

	"github.com/codefly-dev/core/agents/contract"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
)

func TestContainerRecoveryRequirementsDrivePreflight(t *testing.T) {
	required := ContainerRecovery()
	require.Equal(t, []string{contract.ContainerRecoveryScope}, required.Capabilities)

	instance := &services.Instance{
		Identity:               &resources.ServiceIdentity{Module: "infra", Name: "db"},
		Info:                   &agentv0.AgentInformation{Contract: required},
		ContainerRecoveryScope: "scope",
	}
	require.NoError(t, RequireContainerRecovery(instance, "scope"))

	missing := ContainerRecovery()
	missing.Capabilities = nil
	instance.Info.Contract = missing
	require.ErrorContains(t, RequireContainerRecovery(instance, "scope"), contract.ContainerRecoveryScope)
}
