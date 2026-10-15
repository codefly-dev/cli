package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/resources"
)

// Every run — a service, a solution or the whole workspace — admits the
// components composed after it started when it receives SIGUSR1, the one
// signal the run leaves free (SIGHUP is ignored so the stack survives a
// closed shell; SIGINT and SIGTERM stop it). The composition is read again
// from disk, its roots compared with the ones the run starts and the ones it
// composed when the run began, and each root neither knew is admitted into
// the native cluster, if one answers, and started by the flow. Nothing
// already running is restarted, and the run's authority is untouched: the
// host keeps its process. A root the composition already named when a
// service or solution run started, outside that run's closure, stays outside
// it: the run's scope is the entry's, and only what is composed after the
// start joins.
// workspaceRun is set by `codefly run workspace`: its roots are the
// composition's, so a root the composition drops is withdrawn from the run.
// A service or solution run pins the roots named on its command line, which
// no reload withdraws.
var workspaceRun bool

// pinnedRoots is what a reload never withdraws: the roots a service or
// solution run was started with. A workspace run pins none.
func pinnedRoots(flow *orchestration.Flow) map[string]bool {
	pinned := map[string]bool{}
	if workspaceRun {
		return pinned
	}
	for _, root := range flow.Roots() {
		pinned[root] = true
	}
	return pinned
}

// goneRoots is the roots the run starts that a reloaded composition no longer
// names, the pinned ones excepted, in the run's order.
func goneRoots(pinned map[string]bool, running, roots []string) []string {
	named := make(map[string]bool, len(roots))
	for _, root := range roots {
		named[root] = true
	}
	var gone []string
	for _, root := range running {
		if !named[root] && !pinned[root] {
			gone = append(gone, root)
		}
	}
	return gone
}

func watchForExtensions(ctx context.Context, flow *orchestration.Flow, baseline map[string]bool) {
	pinned := pinnedRoots(flow)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1)
	defer signal.Stop(signals)
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
			if err := withdrawDroppedRoots(ctx, flow, pinned); err != nil {
				cli.Warning("composition withdrawal is incomplete; the next SIGUSR1 retries it: %v", err)
				continue
			}
			if err := extendRunningWorkspace(ctx, flow, baseline); err != nil {
				cli.Warning("the running composition was not extended: %v", err)
			}
		}
	}
}

// extensionBaseline is what a run knows when it starts: the roots the
// composition names then. A workspace that composes nothing beyond the one
// service being run has no composition roots to name, and the run's own
// roots are the baseline.
func extensionBaseline(ctx context.Context, workspace *resources.Workspace) map[string]bool {
	baseline := map[string]bool{}
	roots, err := CompositionRoots(ctx, workspace)
	if err != nil {
		return baseline
	}
	for _, root := range roots {
		baseline[root] = true
	}
	return baseline
}

// freshRoots is the roots a reloaded composition names that neither the
// baseline nor the run knows, in the composition's order.
func freshRoots(known map[string]bool, roots []string) []string {
	var fresh []string
	for _, root := range roots {
		if !known[root] {
			fresh = append(fresh, root)
		}
	}
	return fresh
}

// withdrawDroppedRoots reloads the workspace and withdraws from the run every
// root it starts that the composition no longer names, the pinned ones
// excepted: the flow stops what only those roots needed, dependents first,
// and the native cluster, if one answers, takes each stopped workload out of
// its module's manifest — withdrawing the module when nothing of it runs any
// more — and forgets it. The host keeps its process; the envelope is untouched.
func withdrawDroppedRoots(ctx context.Context, flow *orchestration.Flow, pinned map[string]bool) error {
	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("reload the workspace: %w", err)
	}
	roots, err := CompositionRoots(ctx, workspace)
	if err != nil {
		return err
	}
	gone := goneRoots(pinned, flow.Roots(), roots)
	var stopped []string
	if len(gone) > 0 {
		stopped, err = flow.Withdraw(ctx, workspace, gone)
	}
	for _, unique := range stopped {
		cli.Info("stopped %s", unique)
	}
	if cluster := activeNativeCluster(); cluster != nil {
		withdrawn, withdrawErr := cluster.withdraw(ctx, stopped)
		for _, module := range withdrawn {
			cli.Info("withdrew %s from the host: its module manifest is a withdrawal now, terminal for that binding", module)
		}
		if withdrawErr != nil {
			return errors.Join(err, withdrawErr)
		}
	}
	return err
}

// extendRunningWorkspace reloads the workspace and admits into the run every
// root the composition now names that neither the run nor its baseline knows:
// its derived run inputs join the run's, the native cluster projects its
// identity, and the flow starts its closure.
func extendRunningWorkspace(ctx context.Context, flow *orchestration.Flow, baseline map[string]bool) error {
	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("reload the workspace: %w", err)
	}
	roots, err := CompositionRoots(ctx, workspace)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(baseline)+len(roots))
	for root := range baseline {
		known[root] = true
	}
	for _, root := range flow.Roots() {
		known[root] = true
	}
	fresh := freshRoots(known, roots)
	merged := solutionrun.RunInputs{Overrides: derivedOverrides}
	for _, root := range fresh {
		info, parseErr := resources.ParseServiceWithOptionalModule(root)
		if parseErr != nil {
			return parseErr
		}
		module, loadErr := workspace.LoadModuleFromName(ctx, info.Module)
		if loadErr != nil {
			return loadErr
		}
		service, loadErr := module.LoadServiceFromName(ctx, info.Name)
		if loadErr != nil {
			return loadErr
		}
		derived, deriveErr := solutionrun.DerivedRunInputs(module, service, root)
		if deriveErr != nil {
			return deriveErr
		}
		merged = solutionrun.Merge(merged, derived)
	}
	for _, note := range merged.Notes {
		if note.Warning {
			cli.Warning("%s", note.Message)
			continue
		}
		cli.Info("%s", note.Message)
	}
	if cluster := activeNativeCluster(); cluster != nil {
		admitted, admitErr := cluster.admit(ctx, workspace)
		if admitErr != nil {
			return admitErr
		}
		for _, unique := range admitted {
			cli.Info("admitted %s: its identity is projected, and it is delivered to the host once running", unique)
		}
	}
	// Even without new roots, admit refreshes the declaration snapshot used
	// by the next agent Running hook. Otherwise edits such as regrouping
	// remain stuck at the composition that started the run.
	if len(fresh) == 0 {
		cli.Info("refreshed the composition; no new root to start")
		return nil
	}
	derivedOverrides = merged.Overrides
	flow.WithOverrides(derivedOverrides)
	added, err := flow.Extend(ctx, workspace, fresh)
	if err != nil {
		return err
	}
	for _, root := range added {
		cli.Info("started %s", root)
	}
	return nil
}

// activeNativeCluster is the native cluster of this process's run, if one
// answers as the composition's cluster.
func activeNativeCluster() *nativeCluster {
	nativeClusterMu.Lock()
	defer nativeClusterMu.Unlock()
	return nativeClusterActive
}
