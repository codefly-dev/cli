package orchestration

import (
	"fmt"
	"os"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
)

// ContainerRecoveryScope resolves this flow's ownership identity and projects it
// into the environment every agent the flow spawns inherits. It lives here,
// under InitManagers, rather than in the commands: an agent reaches Docker
// through its Core companion on every entry point that spawns one — build,
// test, ci, deploy, sync, validation, gitops and the control plane, not only
// run — and a container created while the marker is missing carries no recovery
// label, so no sweep on any Core generation can ever match it.
//
// Projection is process-global (an environment variable) and the identity is
// fixed by the flow's workspace and naming scope, so it is resolved once and
// reused: the sweep asks for it before InitManagers does.
func (flow *Flow) ContainerRecoveryScope() (dockerrun.ContainerRecoveryScope, error) {
	if flow.containerRecoveryScope != (dockerrun.ContainerRecoveryScope{}) {
		return flow.containerRecoveryScope, nil
	}
	home := resources.CodeflyHomeDir()
	if err := os.MkdirAll(home, 0o700); err != nil {
		return dockerrun.ContainerRecoveryScope{}, fmt.Errorf("prepare container recovery home: %w", err)
	}
	var namingScope string
	if env := flow.Environment(); env != nil {
		namingScope = env.NamingScope
	}
	scope, err := dockerrun.NewContainerRecoveryScope(home, flow.workspace.Dir(), namingScope)
	if err != nil {
		return scope, fmt.Errorf("resolve container recovery ownership: %w", err)
	}
	if err := dockerrun.SetContainerRecoveryScope(scope); err != nil {
		return scope, err
	}
	flow.containerRecoveryScope = scope
	return scope, nil
}

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
