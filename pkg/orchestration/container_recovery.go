package orchestration

import (
	"fmt"
	"os"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
)

// A CLI dependency bump says nothing about the separately compiled agent.
// Require its acknowledgement before Init can create any Docker resources.
func (runner *Runner) validateContainerRecovery() error {
	if runner.runtimeContext == resources.RuntimeContextNative || runner.runtimeContext == resources.RuntimeContextNix {
		return nil
	}
	if os.Getenv(dockerrun.ContainerRecoveryScopeEnvironment) == "" {
		return nil
	}
	expected := dockerrun.InheritedContainerRecoveryScope()
	if expected == "" {
		return fmt.Errorf("cannot initialize Docker with an invalid container recovery identity")
	}
	if runner.instance.ContainerRecoveryScope != expected {
		return fmt.Errorf("agent for %s did not acknowledge this run's container recovery scope; rebuild the agent against the CLI's pinned Core before running with Docker (upgrading the CLI alone does not update agent binaries); existing unlabeled containers require explicit recovery by container ID", runner.instance.Unique())
	}
	return nil
}
