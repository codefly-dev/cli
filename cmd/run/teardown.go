package run

import (
	"context"
	"slices"
	"time"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
)

// reclaimBudget bounds what a run's exit spends taking back what its agents
// left: Core's recovery gives each orphaned group a SIGTERM grace before it
// escalates, so a few of them need more than one grace.
const reclaimBudget = 45 * time.Second

// runFailed tells a run that failed from one stopped on purpose (SIGINT or
// SIGTERM, which cancel signalled) or one that finished.
func runFailed(signalled context.Context, err error) bool {
	return err != nil && signalled.Err() == nil
}

// removesContainers decides whether a run's exit removes the containers its
// agents created. Only a failed run does (#1233): a run stopped on purpose
// keeps them for the next run to reuse — Core's sweep removes a stopped
// container whose owner is gone, so even stopping them would lose that. An
// explicit Local/Nix run never touches the Docker control plane.
func removesContainers(failed bool, runtimeContexts []string) bool {
	return failed && slices.ContainsFunc(runtimeContexts, shouldSweepStaleContainers)
}

// reclaimRun takes back what the run's agents started and left behind once
// they have exited: their orphaned process groups whichever way the run
// ended, and, for a failed run, the containers they created and the networks
// those leave empty.
func reclaimRun(ctx context.Context, flow *orchestration.Flow, started time.Time, failed bool) error {
	if flow == nil {
		return nil
	}
	reclaimCtx, cancel := context.WithTimeout(ctx, reclaimBudget)
	defer cancel()
	reclaimed, err := flow.ReclaimAfterRun(reclaimCtx, started, removesContainers(failed, flow.SelectableRuntimeContexts()))
	if len(reclaimed.ProcessGroups) > 0 {
		cli.Info("stopped %d process groups this run's agents left running: %v", len(reclaimed.ProcessGroups), reclaimed.ProcessGroups)
	}
	if len(reclaimed.Containers) > 0 {
		cli.Info("removed %d containers the failed run started: %v", len(reclaimed.Containers), reclaimed.Containers)
	}
	if len(reclaimed.Networks) > 0 {
		cli.Info("removed %d networks the failed run's containers left empty: %v", len(reclaimed.Networks), reclaimed.Networks)
	}
	return err
}
