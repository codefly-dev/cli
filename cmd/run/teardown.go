package run

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/processgroup"
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

// removesContainers decides whether the exit of a run a developer can run
// again removes the containers its agents created. Only a failed one does
// (#1233): a run stopped on purpose keeps them for the next run to reuse —
// Core's sweep removes a stopped container whose owner is gone, so even
// stopping them would lose that. An explicit Local/Nix run never touches the
// Docker control plane.
//
// A disposable run is not decided here: nothing runs its scope again, so its
// containers, their volumes and their data directories go however it ended
// (Flow.RemoveContainersOfThisDisposableRun, on the run's Disposable fact).
func removesContainers(failed bool, runtimeContexts []string) bool {
	return failed && slices.ContainsFunc(runtimeContexts, shouldSweepStaleContainers)
}

// reclaimRun takes back what the run's agents started and left behind once
// they have exited: whichever way the run ended, their orphaned process
// groups, the native state directories and the containers of a disposable run
// and the networks its containers left empty, and, for a failed run, the
// containers they created.
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
	if len(reclaimed.NativeState) > 0 {
		cli.Info("removed %d native state directories of this disposable run: %v", len(reclaimed.NativeState), reclaimed.NativeState)
	}
	if len(reclaimed.DisposableContainers) > 0 {
		cli.Info("removed %d containers of this disposable run, with their volumes: %v", len(reclaimed.DisposableContainers), reclaimed.DisposableContainers)
	}
	if len(reclaimed.ContainerData) > 0 {
		cli.Info("removed %d container data directories of this disposable run: %v", len(reclaimed.ContainerData), reclaimed.ContainerData)
	}
	if len(reclaimed.Containers) > 0 {
		cli.Info("removed %d containers the failed run started: %v", len(reclaimed.Containers), reclaimed.Containers)
	}
	if len(reclaimed.Networks) > 0 {
		cli.Info("removed %d networks this run's containers left empty: %v", len(reclaimed.Networks), reclaimed.Networks)
	}
	return err
}

// takeBackOnForcedQuit is what a forced quit still does before the process
// exits: for a disposable run, its agents are asked to stop and what it left
// is taken back (Flow.TakeBackOnForcedQuit); for any other run, nothing. ctx
// is the forced quit's own short budget.
func takeBackOnForcedQuit(ctx context.Context, flow *orchestration.Flow, started time.Time) {
	if flow == nil {
		return
	}
	reclaimed, err := flow.TakeBackOnForcedQuit(ctx, started)
	if len(reclaimed.ProcessGroups) > 0 {
		cli.Info("stopped %d process groups this disposable run's agents left running: %v", len(reclaimed.ProcessGroups), reclaimed.ProcessGroups)
	}
	if len(reclaimed.NativeState) > 0 {
		cli.Info("removed %d native state directories of this disposable run: %v", len(reclaimed.NativeState), reclaimed.NativeState)
	}
	if len(reclaimed.DisposableContainers) > 0 {
		cli.Info("removed %d containers of this disposable run, with their volumes: %v", len(reclaimed.DisposableContainers), reclaimed.DisposableContainers)
	}
	if err != nil {
		cli.Warning("the forced quit left part of this disposable run behind; `codefly clear` removes it: %v", err)
	}
}

// reapBudget bounds the sweep a run makes as it exits.
const reapBudget = 30 * time.Second

// reapAfterRun recovers the process groups whose owner has exited, on the whole
// machine: garbage collection, which never signals a live workspace's run.
//
// A group the sweep cannot decide about is kept either way. It is this run's
// error only when one of this run's agents started it; any other is reported
// and left to whoever runs it, because an agent built on an older Core
// registers such a group for as long as it runs, anywhere on the machine, and
// that says nothing about how this run ended.
func reapAfterRun(ctx context.Context, flow *orchestration.Flow) error {
	reapCtx, cancel := context.WithTimeout(ctx, reapBudget)
	defer cancel()
	evidence, kept, err := processgroup.ReapStaleProcessGroupsAfterRun(reapCtx, func(pgid int) bool {
		return flow.OwnsProcessGroup(reapCtx, pgid)
	})
	for _, group := range kept {
		cli.Warning("a process group that is not this run's was kept after run: %v", group)
	}
	if err != nil {
		return fmt.Errorf("reap managed processes after run: %w", err)
	}
	if recovered := evidence.RecoveredPGIDs(); len(recovered) > 0 {
		cli.Info("reaped %d managed process groups after run: %v", len(recovered), recovered)
	}
	return nil
}
