package orchestration

import (
	"fmt"
	"os"
	"sync"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/runners/recoveryscope"
	"github.com/codefly-dev/core/services"
)

// containerRecoveryProjection serializes projecting the marker with the agent
// spawning that inherits it. The marker is a single process-global environment
// variable while flows are not process-global (pkg/engine.FlowManager holds
// several at once), so a second flow projecting between this flow's projection
// and its spawns would hand these agents an ownership no sweep of this flow can
// match.
var containerRecoveryProjection sync.Mutex

// ContainerRecoveryScope resolves this flow's ownership identity and projects it
// into the environment every agent the flow spawns inherits. It lives here,
// under InitManagers, rather than in the commands: an agent reaches Docker
// through its Core companion on every entry point that spawns one — build,
// test, ci, deploy, sync, validation, gitops and the control plane, not only
// run — and a container created while the marker is missing carries no recovery
// label, so no sweep on any Core generation can ever match it.
func (flow *Flow) ContainerRecoveryScope() (dockerrun.ContainerRecoveryScope, error) {
	containerRecoveryProjection.Lock()
	defer containerRecoveryProjection.Unlock()
	return flow.projectContainerRecovery()
}

// projectContainerRecovery requires containerRecoveryProjection to be held.
// Re-projecting an already resolved scope is not redundant: another flow in
// this process may have overwritten the variable since, and agents spawned
// after that point would inherit the other flow's ownership.
func (flow *Flow) projectContainerRecovery() (dockerrun.ContainerRecoveryScope, error) {
	if flow.containerRecoveryIdentity != "" {
		if flow.world != nil {
			flow.world.containerRecoveryIdentity = flow.containerRecoveryIdentity
		}
		return flow.containerRecoveryScope, dockerrun.SetContainerRecoveryScope(flow.containerRecoveryScope)
	}
	scope, err := ContainerRecoveryScopeFor(flow.workspace, flow.Environment())
	if err != nil {
		return scope, err
	}
	if err := dockerrun.SetContainerRecoveryScope(scope); err != nil {
		return scope, err
	}
	// The identity agents echo back, captured from our own projection so that
	// validation never has to re-read a variable another flow can overwrite.
	identity := recoveryscope.Acknowledgement()
	if identity == "" {
		return scope, fmt.Errorf("projected container recovery identity is unreadable")
	}
	flow.containerRecoveryScope, flow.containerRecoveryIdentity = scope, identity
	if flow.world != nil {
		flow.world.containerRecoveryIdentity = identity
	}
	return scope, nil
}

// ContainerRecoveryScopeFor resolves the container-recovery ownership identity
// from a workspace and the environment a command runs against.
//
// This is deliberately the only implementation of that recipe. The identity is a
// hash of home, workspace and naming scope, and every site that projects a
// marker has to produce a byte-identical one or the containers it labels are
// collected by no sweep at all. A second assembly of the same three inputs
// somewhere else would drift silently — nothing fails when a label merely
// matches nothing — so callers outside a flow resolve through here instead of
// rebuilding the triple. `TestContainerRecoveryScopeHasOneResolver` pins that.
//
// Ownership is refused rather than defaulted when either input is missing:
// resolving from a partial context would project a DIFFERENT durable identity
// rather than none, and containers labeled with it match no later sweep.
func ContainerRecoveryScopeFor(workspace *resources.Workspace, env *resources.Environment) (dockerrun.ContainerRecoveryScope, error) {
	if workspace == nil {
		return dockerrun.ContainerRecoveryScope{}, fmt.Errorf("container recovery requires a workspace")
	}
	if env == nil {
		return dockerrun.ContainerRecoveryScope{}, fmt.Errorf("container recovery requires a resolved environment")
	}
	home := resources.CodeflyHomeDir()
	if err := os.MkdirAll(home, 0o700); err != nil {
		return dockerrun.ContainerRecoveryScope{}, fmt.Errorf("prepare container recovery home: %w", err)
	}
	scope, err := dockerrun.NewContainerRecoveryScope(home, workspace.Dir(), env.NamingScope)
	if err != nil {
		return scope, fmt.Errorf("resolve container recovery ownership: %w", err)
	}
	return scope, nil
}

// A CLI dependency bump says nothing about the separately compiled agent.
// Require its acknowledgement before Init can create any Docker resources.
func (runner *Runner) validateContainerRecovery() error {
	if runner.runtimeContext == resources.RuntimeContextNative || runner.runtimeContext == resources.RuntimeContextNix {
		return nil
	}
	// Compare against what THIS flow projected, never the live process
	// variable: a concurrent flow overwrites that variable, while the agent's
	// acknowledgement was captured when it was spawned (and a cached agent's
	// when some earlier flow spawned it). Reading the variable here reports a
	// correctly rebuilt agent as stale.
	return validateContainerRecovery(runner.instance, runner.containerRecoveryIdentity)
}

func validateContainerRecovery(instance *services.Instance, expected string) error {
	if expected == "" {
		return nil
	}
	if instance.ContainerRecoveryScope != expected {
		return fmt.Errorf("agent for %s did not acknowledge this run's container recovery scope; rebuild the agent against the CLI's pinned Core before running with Docker (upgrading the CLI alone does not update agent binaries); existing unlabeled containers require explicit recovery by container ID", instance.Unique())
	}
	return nil
}
