package run

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/agents/helpers/code"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/builders"
	"github.com/codefly-dev/core/wool"
)

var watchFrontends bool

const watchFrontendsUsage = "Watch selected local frontend sources and rebuild signed host bundles (independent of agent hot-reload; refresh the browser after delivery)"

func (cluster *launcher) refreshFrontendWatches(ctx context.Context) error {
	if !cluster.watchFrontends {
		return nil
	}
	seen := map[string]bool{}
	for _, unique := range cluster.flow.LocalAuthorityServices() {
		identity, ok := cluster.identity(unique)
		if !ok || !cluster.delivers(identity) || seen[identity.Module] {
			continue
		}
		seen[identity.Module] = true
		name := identity.Module
		cluster.mu.Lock()
		lane := cluster.laneLocked(name)
		already := lane.watch != nil || lane.terminal || cluster.closed
		cluster.mu.Unlock()
		if already {
			continue
		}
		selection, err := composition.SelectModule(ctx, cluster.composed(), name)
		if err != nil {
			return err
		}
		if selection.Verified {
			continue
		}
		module, err := cluster.composed().LoadModuleFromName(ctx, name)
		if err != nil {
			return err
		}
		frontend, err := solutionrun.DeclaredFrontend(ctx, module)
		if err != nil {
			return err
		}
		if frontend == nil {
			continue
		}
		tree, files, err := solutionrun.FrontendWatchInterest(ctx, module)
		if err != nil {
			return err
		}
		dependencies := builders.NewDependencies("frontend")
		for _, file := range files {
			dependencies.AddDependencies(builders.NewDependency(file))
		}
		base := &services.Base{Location: tree.Root, Wool: wool.Get(ctx).In("frontend.watch", wool.Field("module", name))}
		if err := base.SetupWatcher(ctx, services.NewWatchConfiguration(dependencies).WithTrees(tree), func(code.Change) error { cluster.frontendChanged(ctx, name); return nil }); err != nil {
			return fmt.Errorf("frontend watch %s: %w", name, err)
		}
		cluster.mu.Lock()
		if cluster.closed || lane.terminal || lane.watch != nil {
			base.StopWatcher()
		} else {
			lane.watch = base
		}
		cluster.mu.Unlock()
	}
	return nil
}

// Observation never waits for a build. The existing module gate serializes all
// producers; scheduled only coalesces source notifications into one caller.
func (cluster *launcher) frontendChanged(ctx context.Context, module string) {
	cluster.mu.Lock()
	lane := cluster.laneLocked(module)
	if cluster.closed || lane.terminal {
		cluster.mu.Unlock()
		return
	}
	cluster.invalidateLocked(module)
	if !lane.admitted {
		cluster.mu.Unlock()
		wool.Get(ctx).Info("frontend edit recorded; it can repair a pending admission, otherwise restart the owning run", wool.Field("module", module))
		return
	}
	cluster.scheduleFrontendLocked(ctx, module, lane)
	cluster.mu.Unlock()
}

func (cluster *launcher) scheduleFrontendLocked(ctx context.Context, module string, lane *moduleLane) {
	if lane.scheduled || cluster.closed || lane.terminal {
		return
	}
	lane.scheduled = true
	cluster.active.Add(1)
	go func() {
		defer cluster.active.Done()
		w := wool.Get(ctx).In("frontend.reload", wool.Field("module", module))
		var attempted uint64
		err := hostDelivery{schedule: cluster.schedule, stop: cluster.stop, ready: cluster.hostReady, log: w,
			deliver: func(ctx context.Context) error {
				cluster.mu.Lock()
				attempted = lane.revision
				cluster.mu.Unlock()
				return cluster.reconcileReadiness(ctx, module, false, true)
			},
		}.run(context.WithoutCancel(ctx))
		cluster.mu.Lock()
		lane.scheduled = false
		// Only a later legitimate edit schedules another bounded caller.
		if attempted != 0 && attempted != lane.revision {
			cluster.scheduleFrontendLocked(ctx, module, lane)
		}
		cluster.mu.Unlock()
		if err != nil && !deliveryAbandoned(err) {
			w.Error("frontend delivery unconfirmed; a later source edit retries", wool.ErrField(err))
		}
	}()
}
