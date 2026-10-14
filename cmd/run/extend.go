package run

import (
	"context"
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

// extendOnSignal is set by `codefly run workspace`: a running workspace
// admits the components composed after it started when it receives SIGUSR1,
// the one signal the run leaves free (SIGHUP is ignored so the stack survives
// a closed shell; SIGINT and SIGTERM stop it). The composition is read again
// from disk, its roots compared with the run's, and each root the run did
// not know is admitted into the native cluster, if one answers, and started
// by the flow. Nothing already running is restarted, and the run's authority
// is untouched: the host keeps its process.
var extendOnSignal bool

func watchForExtensions(ctx context.Context, flow *orchestration.Flow) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1)
	defer signal.Stop(signals)
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
			if err := extendRunningWorkspace(ctx, flow); err != nil {
				cli.Warning("the running composition was not extended: %v", err)
			}
		}
	}
}

// extendRunningWorkspace reloads the workspace and admits into the run every
// root the composition now names that the run does not start: its derived run
// inputs join the run's, the native cluster projects its identity, and the
// flow starts its closure.
func extendRunningWorkspace(ctx context.Context, flow *orchestration.Flow) error {
	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("reload the workspace: %w", err)
	}
	roots, err := CompositionRoots(ctx, workspace)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, root := range flow.Roots() {
		known[root] = true
	}
	var fresh []string
	for _, root := range roots {
		if !known[root] {
			fresh = append(fresh, root)
		}
	}
	if len(fresh) == 0 {
		cli.Info("the composition names no component this run does not already start")
		return nil
	}
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
	derivedOverrides = merged.Overrides
	flow.WithOverrides(derivedOverrides)
	if cluster := activeNativeCluster(); cluster != nil {
		admitted, admitErr := cluster.admit(ctx, workspace)
		if admitErr != nil {
			return admitErr
		}
		for _, unique := range admitted {
			cli.Info("admitted %s: its identity is projected, and it is delivered to the host once running", unique)
		}
	}
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
