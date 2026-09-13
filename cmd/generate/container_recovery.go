package generate

import (
	"context"
	"sync"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/runners/dockerrun"
)

// containerRecoveryOnce holds the projection to one resolution per command.
// `generate contracts` and `generate client` build a descriptor set per
// endpoint, so buildDescriptorSet runs once for every grpc endpoint in the
// module — otherwise re-loading the workspace, re-creating the home directory
// and re-emitting the degradation warning on each one. The enclosing workspace
// cannot change inside a single invocation, so resolving once is both the
// cheaper and the more honest shape: outside a workspace the warning is stated
// once, not once per endpoint.
var containerRecoveryOnce sync.Once

// projectContainerRecovery stamps this process's container-recovery ownership
// before `generate` builds a container.
//
// Every other command that reaches Docker does so through an agent, and
// Flow.InitManagers projects for those. `generate` builds its containers in the
// CLI process itself, so no flow ever projects for it and the proto companion
// creates them with no recovery label — unrecoverable by any sweep, on any Core
// generation, which rebuilding the agent cannot fix.
//
// The identity comes from orchestration.ContainerRecoveryScopeFor, the same
// call a flow projects from, so a run in this workspace resolves byte-identical
// ownership and its exact-scope sweep matches what generate labeled.
//
// That sweep alone would not be enough. It compares a hash that includes the
// naming scope, and a run is free to choose a different one — `--naming-scope`,
// a non-local `--env`, or the invocation id `--temporary-ports` generates — in
// which case the label matches nothing and the container is collected by no
// one. The containers generate builds are pure throwaways, so the call sites
// also mark them ephemeral, which brings them under the durable-namespace
// sweep (ReapDisposableContainers). That namespace covers home and workspace
// only, so any later run in this workspace collects a leftover whatever naming
// scope it picked.
func projectContainerRecovery(ctx context.Context) {
	containerRecoveryOnce.Do(func() {
		scope, err := containerRecoveryScope(ctx)
		if err == nil {
			err = dockerrun.SetContainerRecoveryScope(scope)
		}
		if err != nil {
			// A directory outside any workspace, an unwritable home, no readable
			// PID namespace: these all have to keep generating. Core degrades the
			// same conditions to "no durable identity" rather than refusing to run,
			// and InitManagers warns and continues for exactly this reason. Say so
			// loudly and create unlabeled containers, as this command did before.
			cli.Warning("cannot project container recovery ownership: containers this command creates will not be recoverable by scope (%v)", err)
		}
	})
}

// containerRecoveryScope resolves the ownership identity without projecting it.
// Resolution and projection are separate so the identity can be asserted
// against what a run would produce.
func containerRecoveryScope(ctx context.Context) (dockerrun.ContainerRecoveryScope, error) {
	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return dockerrun.ContainerRecoveryScope{}, err
	}
	env, err := orchestration.SelectEnvironment(workspace, orchestration.LocalEnvironmentName)
	if err != nil {
		return dockerrun.ContainerRecoveryScope{}, err
	}
	return orchestration.ContainerRecoveryScopeFor(workspace, env)
}
