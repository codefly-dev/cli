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
	for _, unique := range cluster.flow.LocalAuthorityServices() {
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
	// One snapshot of the selection decides both what is admitted and which
	// binding targets are part of the run.
	modules, selected := map[string]bool{}, map[string]bool{}
	for _, unique := range cluster.flow.LocalAuthorityServices() {
		if ref, err := resources.ParseServiceWithOptionalModule(unique); err == nil {
			selected[ref.Module] = true
		}
		if identity, ok := cluster.identity(unique); ok && cluster.delivers(identity) {
			modules[identity.Module] = true
		}
	}
	names := make([]string, 0, len(modules))
	for module := range modules {
		names = append(names, module)
	}
	sort.Strings(names)
	// A declared page is part of what a module is: the authority admits a
	// module's roles only beside its page, so admission names the page's
	// bundle as a ready delivery does. It is built here, before the delivery
	// lock, so a build delays no other module's delivery; the ready delivery
	// reuses the bundle retained for unchanged inputs.
	loadedModules := make([]*resources.Module, len(names))
	bundles := make([]*solutionhost.FrontendPayload, len(names))
	for n, module := range names {
		loaded, err := cluster.composed().LoadModuleFromName(ctx, module)
		if err != nil {
			return err
		}
		if bundles[n], err = cluster.frontendBundle(ctx, loaded); err != nil {
			return err
		}
		loadedModules[n] = loaded
	}
	if err := cluster.acquireDelivery(ctx); err != nil {
		return err
	}
	defer cluster.delivery.Release(1)
	if err := cluster.checkRoutingSnapshot(ctx); err != nil {
		return err
	}
	deliverer, gatewayURL, err := cluster.newDeliverer(ctx)
	if err != nil {
		return err
	}
	manifests := make([]*solutionhost.ModuleManifest, 0, len(names))
	for n, module := range names {
		manifest, err := cluster.renderDelivery(ctx, deliverer, loadedModules[n], true, bundles[n])
		if err != nil {
			return err
		}
		if err := cluster.checkBindingTargets(ctx, module, manifest, selected); err != nil {
			return err
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

// checkBindingTargets decides, at admission, a module's bindings by where they
// point. A binding to the host's own module could never be served, since the
// run declares that module to no authority, so admission refuses it before
// any manifest is sent. Every other binding is delivered as declared: a run
// composes a unit and only the modules it consumes, and the authority decides
// a binding at call time, serving it when its audience is composed (by this
// run, or by the fixture's stand-in for a process the run does not supervise)
// and refusing a call through it by name when nothing composes it. A binding
// to a module outside the run is said once, since admission repeats per start.
// Caller holds delivery.
func (cluster *launcher) checkBindingTargets(ctx context.Context, module string, manifest *solutionhost.ModuleManifest, selected map[string]bool) error {
	if manifest.Authority == nil {
		return nil
	}
	for _, binding := range manifest.Authority.Bindings {
		if binding.Audience == cluster.hostModule {
			return fmt.Errorf("module %s binding %s targets %s, the host's own module, which this run declares to no authority; authority admission requires its target declaration", module, binding.ID, binding.Audience)
		}
		key := module + "/" + binding.ID
		if selected[binding.Audience] || cluster.outsideTargets[key] {
			continue
		}
		if cluster.outsideTargets == nil {
			cluster.outsideTargets = map[string]bool{}
		}
		cluster.outsideTargets[key] = true
		wool.Get(ctx).In("launcher.admit").Warn(
			fmt.Sprintf("module %s binding %s targets %s, which is not part of this run: it is delivered as declared, and the authority refuses a call through it (audience %q is not composed) unless something else composes %s, such as a fixture's stand-in", module, binding.ID, binding.Audience, binding.Audience, binding.Audience),
			wool.Field("module", module), wool.Field("binding", binding.ID), wool.Field("target", binding.Audience))
	}
	return nil
}

// The dependency SDK drives this process itself. Admit its declared workload,
// but leave it unready: the launcher neither starts it nor observes a listener.
func (cluster *launcher) prepareExternalWorkload(ctx context.Context, service *resources.Service) (func(), error) {
	unique, declared, ok := cluster.identityOf(service)
	if !ok {
		return nil, fmt.Errorf("excluded root has no declared workload identity")
	}
	if _, err := cluster.project(unique, declared); err != nil {
		return nil, err
	}
	abandon := func() { cluster.vouch.Forget(unique) }
	err := hostDelivery{
		schedule: cluster.schedule, stop: cluster.stop,
		ready: cluster.hostReady, deliver: cluster.admitSelected,
		log: wool.Get(ctx).In("launcher.external"),
	}.run(ctx)
	if err != nil {
		abandon()
		return nil, err
	}
	return abandon, nil
}
