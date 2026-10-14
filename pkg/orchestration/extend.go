package orchestration

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/tui"
	"github.com/codefly-dev/core/wool"
)

// extendWaitAttempts and extendWaitInterval bound how long an admitted
// service may wait on a dependency during an extension before it is refused:
// its dependencies are started before it, so a wait here is a dependency not
// yet ready rather than one never started.
const (
	extendWaitAttempts = 60
	extendWaitInterval = time.Second
)

// Roots are the services this run starts as roots, origin first, the
// co-roots after, in the order they were declared or admitted.
func (flow *Flow) Roots() []string {
	return flow.rootUniques()
}

// Extend admits into a running flow the roots a reloaded workspace composes
// that this run did not know when it started, and starts them: their
// dependency closure joins the graph, the managers of the services not yet
// managed are spawned and registered, the configuration manager admits them,
// the live start policy follows the graph, and each new service is driven
// through Load, Init and Start in dependency order, so the same Running hook
// that delivered every other workload delivers these. Nothing already
// running is restarted: a running service's own actions never fan out from
// here. It returns the roots admitted; a workspace that adds no root extends
// nothing. The composition is replaced in place, because the configuration
// manager and its local reader read the pointer the run started with and
// core offers no seam to point them elsewhere.
func (flow *Flow) Extend(ctx context.Context, workspace *resources.Workspace, roots []string) ([]string, error) {
	w := wool.Get(ctx).In("flow.Extend")
	if flow.world == nil || flow.world.Mode != RunMode {
		return nil, fmt.Errorf("a run is extended in run mode only")
	}
	if flow.playbook == nil {
		return nil, fmt.Errorf("the run has not started; a composition is extended once it runs")
	}
	if workspace == nil || flow.workspace == nil {
		return nil, fmt.Errorf("a run is extended with the reloaded workspace")
	}
	flow.extendMu.Lock()
	defer flow.extendMu.Unlock()
	added, err := flow.admitRoots(ctx, workspace, roots)
	if err != nil || len(added) == 0 {
		return nil, err
	}
	remotes, fresh, err := flow.rebuildExtendedGraph(ctx)
	if err != nil {
		return nil, err
	}
	if len(fresh) == 0 {
		return added, nil
	}
	if err = flow.spawnAdmitted(ctx, fresh, remotes); err != nil {
		return nil, err
	}
	for _, unique := range fresh {
		if err = flow.startAdmitted(ctx, unique); err != nil {
			return nil, err
		}
	}
	w.Info("extended the running composition", wool.Field("roots", added), wool.Field("started", fresh))
	return added, nil
}

// Withdraw removes from a running flow the co-roots a reloaded workspace no
// longer composes and stops what only they needed: the services of their
// closure that no remaining root's closure reaches are stopped, dependents
// first, and leave the hub, while the graph, the module seeds and the live
// start policy are rebuilt from the roots that remain. It returns the
// services it stopped, dependents first. The origin service is the run's own
// and is never withdrawn; a root the run does not start is nothing to
// withdraw. Nothing that remains is restarted.
func (flow *Flow) Withdraw(ctx context.Context, workspace *resources.Workspace, roots []string) ([]string, error) {
	w := wool.Get(ctx).In("flow.Withdraw")
	if flow.world == nil || flow.world.Mode != RunMode {
		return nil, fmt.Errorf("a run is narrowed in run mode only")
	}
	if flow.playbook == nil {
		return nil, fmt.Errorf("the run has not started; a composition is narrowed once it runs")
	}
	if workspace == nil || flow.workspace == nil {
		return nil, fmt.Errorf("a run is narrowed with the reloaded workspace")
	}
	flow.extendMu.Lock()
	defer flow.extendMu.Unlock()
	dropped := map[string]bool{}
	for _, root := range roots {
		dropped[root] = true
	}
	var remaining, withdrawn []string
	for _, root := range flow.coRoots {
		if dropped[root] {
			withdrawn = append(withdrawn, root)
			continue
		}
		remaining = append(remaining, root)
	}
	if len(withdrawn) == 0 {
		return nil, nil
	}
	before := flow.OrderedServiceUniques()
	origin, err := resources.ParseServiceWithOptionalModule(resources.WithUnique(flow.originService).Unique())
	if err != nil {
		return nil, err
	}
	seeds := []string{origin.Module}
	for _, root := range remaining {
		info, parseErr := resources.ParseServiceWithOptionalModule(root)
		if parseErr != nil {
			return nil, parseErr
		}
		if info.Module != "" && !slices.Contains(seeds, info.Module) {
			seeds = append(seeds, info.Module)
		}
	}
	*flow.workspace = *workspace
	if flow.graphWorkspace != nil {
		closure, closureErr := runModuleClosure(ctx, flow.workspace, seeds)
		if closureErr != nil {
			return nil, closureErr
		}
		flow.graphWorkspace = closure
	}
	flow.moduleSeeds = seeds
	flow.coRoots = remaining
	if _, _, err = flow.rebuildExtendedGraph(ctx); err != nil {
		return nil, err
	}
	// What the remaining roots still reach is the rebuilt closure, not the
	// flow's service list, which only ever grew.
	required, err := flow.runClosure(ctx)
	if err != nil {
		return nil, err
	}
	kept := map[string]bool{resources.WithUnique(flow.originService).Unique(): true}
	for _, service := range required {
		kept[service.Unique] = true
	}
	managers := map[string]IManager{}
	for _, manager := range flow.hub.all() {
		managers[manager.Unique()] = manager
	}
	var stopped []string
	for i := len(before) - 1; i >= 0; i-- {
		unique := before[i]
		manager, managed := managers[unique]
		if kept[unique] || !managed {
			continue
		}
		if _, stopErr := manager.RunnerDoStop(ctx); stopErr != nil {
			return stopped, fmt.Errorf("%s stop: %w", unique, stopErr)
		}
		flow.hub.remove(unique)
		stopped = append(stopped, unique)
	}
	flow.services = slices.DeleteFunc(flow.services, func(service *resources.Service) bool {
		return service != nil && !kept[resources.WithUnique(service).Unique()]
	})
	w.Info("narrowed the running composition", wool.Field("roots", withdrawn), wool.Field("stopped", stopped))
	return stopped, nil
}

// admitRoots takes the reloaded composition in and records the roots it adds:
// the workspace is replaced in place, the module closure resolved again with
// the modules the new roots bring, and the roots joined to the run's.
func (flow *Flow) admitRoots(ctx context.Context, workspace *resources.Workspace, roots []string) ([]string, error) {
	known := map[string]bool{}
	for _, root := range flow.rootUniques() {
		known[root] = true
	}
	var added []string
	for _, root := range roots {
		if !known[root] {
			known[root] = true
			added = append(added, root)
		}
	}
	if len(added) == 0 {
		return nil, nil
	}
	seeds := slices.Clone(flow.moduleSeeds)
	for _, root := range added {
		info, err := resources.ParseServiceWithOptionalModule(root)
		if err != nil {
			return nil, err
		}
		if info.Module != "" && !slices.Contains(seeds, info.Module) {
			seeds = append(seeds, info.Module)
		}
	}
	*flow.workspace = *workspace
	if flow.graphWorkspace != nil {
		closure, err := runModuleClosure(ctx, flow.workspace, seeds)
		if err != nil {
			return nil, err
		}
		flow.graphWorkspace = closure
	}
	flow.moduleSeeds = seeds
	flow.coRoots = append(flow.coRoots, added...)
	return added, nil
}

// rebuildExtendedGraph rebuilds the dependency graph over the admitted
// composition, scopes it to the run, points the live policy at it and
// returns the remotes and the services the run does not manage yet, in
// dependency order.
func (flow *Flow) rebuildExtendedGraph(ctx context.Context) (map[string]*Remote, []string, error) {
	remotes, options := flow.dependencyGraphOptions()
	dependencies, err := architecture.NewServiceDependencies(ctx, flow.dependencyWorkspace(), options...)
	if err != nil {
		return nil, nil, err
	}
	flow.world.Dependencies = dependencies
	if flow.SharedState != nil {
		flow.SharedState.SetDependencies(dependencies)
	}
	if err = flow.selectDependencyStage(); err != nil {
		return nil, nil, err
	}
	required, err := flow.runClosure(ctx)
	if err != nil {
		return nil, nil, err
	}
	all := make([]string, 0, len(required))
	for _, service := range required {
		all = append(all, service.Unique)
	}
	if err = flow.scopeDependenciesToRun(ctx, all, options); err != nil {
		return nil, nil, err
	}
	managed := map[string]bool{}
	for _, manager := range flow.hub.all() {
		managed[manager.Unique()] = true
	}
	var fresh []string
	for _, unique := range all {
		if !managed[unique] {
			fresh = append(fresh, unique)
		}
	}
	if err = flow.validateDependencyEndpointDeclarations(fresh); err != nil {
		return nil, nil, err
	}
	flow.world.setRunProducers(all, flow.originService)
	flow.world.workspaceProducerLookupOnce = sync.Once{}
	if err = flow.checkConfigurationReferences(ctx, fresh); err != nil {
		return nil, nil, err
	}
	// The live policy computes next actions from the graph the run now
	// manages, or a hot reload of an admitted service would end the playbook
	// on a service its graph does not know.
	if policy, ok := flow.policy.(*RuntimeStartPolicy); ok {
		policy.SetDependencies(flow.world.Dependencies)
	}
	return remotes, fresh, nil
}

// spawnAdmitted spawns the managers of the admitted services, admits them to
// the configuration manager, wires them to the live playbook and resolves
// their backends, as InitManagers and Load do for the run set.
func (flow *Flow) spawnAdmitted(ctx context.Context, fresh []string, remotes map[string]*Remote) error {
	containerRecoveryProjection.Lock()
	managers, identities, err := flow.createManagers(ctx, fresh, remotes)
	containerRecoveryProjection.Unlock()
	if err != nil {
		return err
	}
	if flow.ConfigurationManager != nil {
		if err = flow.ConfigurationManager.Restrict(ctx, identities); err != nil {
			return err
		}
		if err = flow.ConfigurationManager.Load(ctx, flow.world.Env.Runtime()); err != nil {
			return err
		}
	}
	for _, manager := range managers {
		manager.DoSetCallback(flow.playbook.Seed)
		manager.DoSetFailureSink(flow.reportFailure)
	}
	return flow.resolveDockerFallback(ctx)
}

// createManagers spawns the run manager of every service named, in order,
// registering each in the hub the moment it is spawned so a failure partway
// still leaves every spawned runner reachable by Stop. It returns the
// managers and the identities of the services they run.
func (flow *Flow) createManagers(ctx context.Context, required []string, remotes map[string]*Remote) ([]IManager, []*resources.ServiceIdentity, error) {
	w := wool.Get(ctx).In("flow.createManagers")
	var managers []IManager
	var identities []*resources.ServiceIdentity
	for _, unique := range required {
		flow.output().RegisterLoggingResource(unique)
		info, err := resources.ParseServiceWithOptionalModule(unique)
		if err != nil {
			return nil, nil, w.Wrap(err)
		}
		w.Debug("creating run manager", wool.Field("for", unique))
		mod, err := flow.workspace.LoadModuleFromName(ctx, info.Module)
		if err != nil {
			return nil, nil, w.Wrap(err)
		}
		svc, err := mod.LoadServiceFromName(ctx, info.Name)
		if err != nil {
			return nil, nil, w.Wrap(err)
		}
		identity, err := svc.Identity()
		if err != nil {
			return nil, nil, w.Wrap(err)
		}
		flow.services = append(flow.services, svc)
		manager, err := flow.spawnManager(ctx, mod, svc, remotes[unique])
		if err != nil {
			return nil, nil, w.Wrap(err)
		}
		flow.hub.add(manager)
		managers = append(managers, manager)
		identities = append(identities, identity)
	}
	return managers, identities, nil
}

// spawnManager creates the manager of one service: the agent is started and
// its runner configured for this run, unless a test supplied a spawner.
func (flow *Flow) spawnManager(ctx context.Context, module *resources.Module, service *resources.Service, remote *Remote) (IManager, error) {
	if flow.spawn != nil {
		return flow.spawn(ctx, module, service)
	}
	manager, err := New(ctx, module, service, flow.world)
	if err != nil {
		return nil, err
	}
	flow.configureRunner(manager.Runner, service)
	if remote != nil {
		manager.Runner.WithRemote(remote.Environment)
	}
	return manager, nil
}

// startAdmitted drives one admitted service through Load, Init and Start,
// each phase reported to the live status as the playbook reports it, then
// Running, which fires the running hook. It never seeds the playbook: an
// admitted service's actions fan out to nothing already running.
func (flow *Flow) startAdmitted(ctx context.Context, unique string) error {
	for _, phase := range []ActionType{RuntimeLoad, RuntimeInit, RuntimeStart} {
		if err := flow.runPhase(ctx, unique, phase); err != nil {
			return err
		}
	}
	flow.emitState(unique, tui.StateRunning, flow.ServicePort(unique))
	return nil
}

func (flow *Flow) runPhase(ctx context.Context, unique string, phase ActionType) error {
	action := Action{Type: phase, Service: unique}
	state, _ := runtimePhase(phase)
	for attempt := 0; ; attempt++ {
		executor, err := flow.GetExecutor(ctx, action)
		if err != nil {
			return err
		}
		flow.emitState(unique, state, flow.ServicePort(unique))
		output, err := executor(ctx)
		if err != nil {
			flow.emitState(unique, tui.StateFailed, 0)
			return fmt.Errorf("%s %s: %w", unique, phase, err)
		}
		if output == nil || !output.Wait {
			return nil
		}
		if attempt >= extendWaitAttempts {
			flow.emitState(unique, tui.StateFailed, 0)
			return fmt.Errorf("%s still waits on a dependency after %s at %s; the extension stops here", unique, time.Duration(extendWaitAttempts)*extendWaitInterval, phase)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(extendWaitInterval):
		}
	}
}
