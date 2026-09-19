// Package agentrequirements owns the agent capabilities required by CLI operations.
package agentrequirements

import (
	"fmt"

	"github.com/codefly-dev/core/agents/contract"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/services"
)

var containerRecoveryCapabilities = []string{contract.ContainerRecoveryScope}

// ContainerRecovery returns the protocol and capabilities required by CLI
// operations that may create containers.
func ContainerRecovery() *agentv0.AgentContract {
	required := contract.Current()
	required.Capabilities = append([]string(nil), containerRecoveryCapabilities...)
	return required
}

// RequireContainerRecovery verifies both the declared operation capability and
// this flow's exact recovery acknowledgement before container work begins.
func RequireContainerRecovery(instance *services.Instance, expected string) error {
	if err := instance.RequireAgentCapabilities(ContainerRecovery().Capabilities...); err != nil {
		return err
	}
	if expected == "" {
		return fmt.Errorf("container recovery requires a resolved scope for %s", instance.Unique())
	}
	if instance.ContainerRecoveryScope != expected {
		return fmt.Errorf("agent for %s implements %s but did not acknowledge this run's container recovery scope", instance.Unique(), contract.ContainerRecoveryScope)
	}
	return nil
}
