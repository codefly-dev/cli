package run

import (
	"context"
	"fmt"
	"sort"

	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/codefly-dev/core/wool"
)

func (cluster *launcher) declaredIdentities(module string) []gitops.NativeIdentity {
	var declared []gitops.NativeIdentity
	for _, unique := range cluster.flow.LocalRuntimeServices() {
		if identity, ok := cluster.identity(unique); ok && identity.Module == module {
			declared = append(declared, identity)
		}
	}
	sort.Slice(declared, func(i, j int) bool { return declared[i].Service < declared[j].Service })
	return declared
}

func (cluster *launcher) beginRuntimeStart(ctx context.Context, service *resources.Service) (*orchestration.RuntimeStartAttempt, error) {
	unique, identity, ok := cluster.identityOf(service)
	if !ok {
		return nil, nil
	}
	cluster.mu.Lock()
	delete(cluster.running, unique)
	waiter := cluster.waitFor(unique)
	var projectErr error
	if cluster.vouch != nil {
		_, projectErr = cluster.project(unique, identity)
	}
	cluster.mu.Unlock()
	finish := func(started bool) {
		cluster.mu.Lock()
		cluster.waitersMu.Lock()
		current := cluster.waiters[unique] == waiter
		if current {
			if started {
				cluster.running[unique] = true
			} else {
				delete(cluster.running, unique)
				if cluster.vouch != nil {
					cluster.vouch.Forget(unique)
				}
			}
		}
		cluster.waitersMu.Unlock()
		cluster.mu.Unlock()
		if current && cluster.delivers(identity) {
			publishCtx := ctx
			if !started {
				publishCtx = context.WithoutCancel(ctx)
			}
			go cluster.runReadyDelivery(publishCtx, unique, identity, waiter, !started)
		} else {
			cluster.doneWaiting(unique, waiter)
		}
	}
	attempt := &orchestration.RuntimeStartAttempt{Complete: finish}
	if cluster.delivers(identity) {
		attempt.Admit = func(ctx context.Context) error {
			return hostDelivery{
				schedule: cluster.schedule, stop: cluster.stop, superseded: waiter.superseded,
				ready: cluster.hostReady, deliver: cluster.admitSelected,
				log: wool.Get(ctx).In("launcher.admit", wool.Field("service", unique)),
			}.run(ctx)
		}
	}
	return attempt, projectErr
}

// Admit the whole selected authority closure before the first consumer starts.
// A binding can target a later process, so declarations precede readiness.
// Failed Start leaves an admitted but unready workload, never a terminal
// withdrawal; normal run teardown stops credential refresh.
func (cluster *launcher) admitSelected(ctx context.Context) error {
	if err := cluster.acquireDelivery(ctx); err != nil {
		return err
	}
	defer cluster.delivery.Release(1)
	modules := map[string]bool{}
	for _, unique := range cluster.flow.LocalRuntimeServices() {
		if identity, ok := cluster.identity(unique); ok && cluster.delivers(identity) {
			modules[identity.Module] = true
		}
	}
	names := make([]string, 0, len(modules))
	for module := range modules {
		names = append(names, module)
	}
	sort.Strings(names)
	deliverer, gatewayURL, err := cluster.newDeliverer(ctx)
	if err != nil {
		return err
	}
	manifests := make([]*solutionhost.ModuleManifest, 0, len(names))
	for _, module := range names {
		loaded, err := cluster.composed().LoadModuleFromName(ctx, module)
		if err != nil {
			return err
		}
		// Admission builds no page: the bundle a ready delivery builds, outside
		// the delivery lock, publishes it.
		manifest, err := cluster.renderDelivery(ctx, deliverer, loaded, true, nil)
		if err != nil {
			return err
		}
		if manifest.Authority != nil {
			for _, binding := range manifest.Authority.Bindings {
				if !modules[binding.Audience] {
					return fmt.Errorf("module %s binding %s targets %s, which has no selected local identity workload; authority admission requires its target declaration", module, binding.ID, binding.Audience)
				}
			}
		}
		manifests = append(manifests, manifest)
	}
	for n, manifest := range manifests {
		if err := cluster.applyDelivery(ctx, deliverer, gatewayURL, names[n], manifest); err != nil {
			return err
		}
	}
	return nil
}
