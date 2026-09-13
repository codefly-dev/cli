package generate

import (
	"context"
	"fmt"
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
)

// projectContainerRecovery stamps this process's container-recovery ownership
// before `generate` builds a container.
//
// Every other command that reaches Docker does so through an agent, and
// Flow.InitManagers projects for those. `generate` builds its containers in the
// CLI process itself, so no flow ever projects for it and the proto companion
// creates them with no recovery label — unrecoverable by any sweep, on any Core
// generation, which rebuilding the agent cannot fix.
//
// The identity must be the one a later `codefly run` resolves, or the label is
// present and still matches nothing: these containers are not ephemeral, so
// only the exact-scope sweep can collect them, and that compares a hash of
// home, workspace and naming scope. Hence the same triple a run takes — the
// enclosing workspace and its `local` environment.
func projectContainerRecovery(ctx context.Context) {
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
	home := resources.CodeflyHomeDir()
	if err = os.MkdirAll(home, 0o700); err != nil {
		return dockerrun.ContainerRecoveryScope{}, fmt.Errorf("prepare container recovery home: %w", err)
	}
	return dockerrun.NewContainerRecoveryScope(home, workspace.Dir(), env.NamingScope)
}
