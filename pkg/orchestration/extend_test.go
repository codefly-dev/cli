package orchestration

import (
	"context"
	"errors"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// phaseRecorder is a manager that records the runtime phases it is driven
// through and answers each as a service that needs nothing more would.
type phaseRecorder struct {
	recordingManager
	phases   []ActionType
	callback func(context.Context, Action) error
	stopErr  error
}

func (m *phaseRecorder) RunnerDoLoad(context.Context) (*OutputProperty, error) {
	m.phases = append(m.phases, RuntimeLoad)
	return &OutputProperty{IndependentUpdate: true}, nil
}

func (m *phaseRecorder) RunnerDoInit(context.Context) (*OutputProperty, error) {
	m.phases = append(m.phases, RuntimeInit)
	return &OutputProperty{IndependentUpdate: true}, nil
}

func (m *phaseRecorder) RunnerDoStart(context.Context) (*OutputProperty, error) {
	m.phases = append(m.phases, RuntimeStart)
	return &OutputProperty{IndependentUpdate: true}, nil
}

func (m *phaseRecorder) RunnerDoStop(context.Context) (*OutputProperty, error) {
	m.phases = append(m.phases, RuntimeStop)
	return nil, m.stopErr
}

func (m *phaseRecorder) DoSetCallback(callback func(context.Context, Action) error) {
	m.callback = callback
}

// A running composition is extended by the roots a reloaded workspace adds:
// the admitted root's closure is spawned and driven through Load, Init and
// Start in dependency order, reaching Running through the same hook as every
// other workload, while nothing the run already managed is touched; the
// graph, the roots and the live policy know the newcomers afterwards, and
// extending with the same composition again does nothing.
func TestExtendStartsOnlyTheAdmittedClosureAndLeavesTheRunAlone(t *testing.T) {
	flow, ctx := multiRootFlow(t, "lastlogin-go/backend")
	required, err := flow.runClosure(ctx)
	require.NoError(t, err)
	flow.hub = &Hub{}
	running := map[string]*phaseRecorder{}
	for _, unique := range append(uniques(required), "lastlogin-go/backend") {
		manager := &phaseRecorder{recordingManager: recordingManager{unique: unique}}
		running[unique] = manager
		flow.hub.add(manager)
	}
	flow.playbook = &Playbook{}
	flow.policy = &RuntimeStartPolicy{dependencies: flow.world.Dependencies, ExecutorManager: flow}
	var spawned []*phaseRecorder
	flow.spawn = func(_ context.Context, _ *resources.Module, service *resources.Service) (IManager, error) {
		manager := &phaseRecorder{recordingManager: recordingManager{unique: resources.WithUnique(service).Unique()}}
		spawned = append(spawned, manager)
		return manager, nil
	}
	var hooked []string
	flow.WithRunningHook(func(service string, _ int) { hooked = append(hooked, service) })

	reloaded, err := resources.LoadWorkspaceFromDir(ctx, "testdata/multi-root")
	require.NoError(t, err)
	added, err := flow.Extend(ctx, reloaded, []string{"wiki/backend", "lastlogin-go/backend"})
	require.NoError(t, err)
	require.Equal(t, []string{"wiki/backend"}, added, "a root the run already starts is not admitted twice")

	var started []string
	for _, manager := range spawned {
		started = append(started, manager.unique)
		require.Equal(t, []ActionType{RuntimeLoad, RuntimeInit, RuntimeStart}, manager.phases, manager.unique)
		require.NotNil(t, manager.callback, "an admitted manager seeds the live playbook on a hot reload")
	}
	require.Equal(t, []string{"documents/documents", "wiki/backend"}, started, "the admitted root's closure, dependencies first, and nothing the run already managed")
	kept := map[IManager]bool{}
	for _, manager := range flow.hub.all() {
		kept[manager] = true
	}
	for unique, manager := range running {
		require.Empty(t, manager.phases, unique+" was running and is left alone")
		require.True(t, kept[manager], unique+" keeps the manager, and so the process, it was running under: the host is not restarted")
	}
	require.Equal(t, started, hooked, "each admitted service reaches Running through the hook every workload reaches it through")
	require.Equal(t, []string{"lastlogin-go/backend", "wiki/backend"}, flow.Roots())
	_, err = flow.ServiceFromUnique("wiki/backend")
	require.NoError(t, err, "the graph knows the admitted root")
	require.Contains(t, flow.OrderedServiceUniques(), "wiki/backend")
	policy, ok := flow.policy.(*RuntimeStartPolicy)
	require.True(t, ok)
	require.Same(t, flow.world.Dependencies, policy.graph(), "the live policy computes next actions from the extended graph")

	again, err := flow.Extend(ctx, reloaded, []string{"wiki/backend"})
	require.NoError(t, err)
	require.Empty(t, again)
	require.Len(t, spawned, 2, "nothing is spawned twice")
}

// Withdrawing a root a reloaded composition dropped stops only what that
// root alone needed, dependents first, and removes it from the hub and the
// graph; what the remaining roots need keeps its manager, and so its process,
// untouched. The origin root is never withdrawn, nor is a root the run does
// not start.
func TestWithdrawStopsOnlyWhatTheDroppedRootNeededAndLeavesTheRunAlone(t *testing.T) {
	flow, ctx := multiRootFlow(t, "lastlogin-go/backend")
	required, err := flow.runClosure(ctx)
	require.NoError(t, err)
	flow.hub = &Hub{}
	running := map[string]*phaseRecorder{}
	for _, unique := range append(uniques(required), "lastlogin-go/backend") {
		manager := &phaseRecorder{recordingManager: recordingManager{unique: unique}}
		running[unique] = manager
		flow.hub.add(manager)
	}
	flow.playbook = &Playbook{}
	flow.policy = &RuntimeStartPolicy{dependencies: flow.world.Dependencies, ExecutorManager: flow}
	spawned := map[string]*phaseRecorder{}
	flow.spawn = func(_ context.Context, _ *resources.Module, service *resources.Service) (IManager, error) {
		manager := &phaseRecorder{recordingManager: recordingManager{unique: resources.WithUnique(service).Unique()}}
		spawned[manager.unique] = manager
		return manager, nil
	}
	flow.WithRunningHook(func(string, int) {})
	reloaded, err := resources.LoadWorkspaceFromDir(ctx, "testdata/multi-root")
	require.NoError(t, err)
	_, err = flow.Extend(ctx, reloaded, []string{"wiki/backend"})
	require.NoError(t, err)
	require.Len(t, spawned, 2)

	stopped, err := flow.Withdraw(ctx, reloaded, []string{"wiki/backend", "lastlogin-go/backend", "nobody/runs-this"})
	require.NoError(t, err)
	require.Equal(t, []string{"wiki/backend", "documents/documents"}, stopped, "the dropped root's closure, dependents first; the origin and an unknown root are nothing to withdraw")
	for _, unique := range stopped {
		require.Equal(t, []ActionType{RuntimeLoad, RuntimeInit, RuntimeStart, RuntimeStop}, spawned[unique].phases, unique)
	}
	kept := map[IManager]bool{}
	for _, manager := range flow.hub.all() {
		kept[manager] = true
	}
	for unique, manager := range running {
		require.Empty(t, manager.phases, unique+" was running and is left alone")
		require.True(t, kept[manager], unique+" keeps the manager it was running under")
	}
	for _, unique := range stopped {
		require.False(t, kept[spawned[unique]], unique+" left the hub")
	}
	require.Equal(t, []string{"lastlogin-go/backend"}, flow.Roots())
	_, err = flow.ServiceFromUnique("wiki/backend")
	require.Error(t, err, "the graph no longer knows the withdrawn root")
	require.NotContains(t, flow.OrderedServiceUniques(), "documents/documents")
	policy, ok := flow.policy.(*RuntimeStartPolicy)
	require.True(t, ok)
	require.Same(t, flow.world.Dependencies, policy.graph(), "the live policy follows the narrowed graph")

	again, err := flow.Withdraw(ctx, reloaded, []string{"wiki/backend"})
	require.NoError(t, err)
	require.Empty(t, again, "withdrawing what is already gone is nothing")
}

// A run that has not started cannot be extended: there is no playbook for an
// admitted manager to seed, so the refusal is by name rather than a later
// panic in the loop.
func TestExtendRefusesARunThatHasNotStarted(t *testing.T) {
	flow, ctx := multiRootFlow(t, "lastlogin-go/backend")
	reloaded, err := resources.LoadWorkspaceFromDir(ctx, "testdata/multi-root")
	require.NoError(t, err)
	_, err = flow.Extend(ctx, reloaded, []string{"wiki/backend"})
	require.ErrorContains(t, err, "has not started")
}

// A failed stop must leave the dropped root admitted so the next reload can
// retry the RPC. Dependents that stopped before the failure stay stopped, but
// the flow must not forget the root and report a false completed withdrawal.
func TestWithdrawRetainsRootsWhenAStopRPCFails(t *testing.T) {
	flow, ctx := multiRootFlow(t, "lastlogin-go/backend")
	required, err := flow.runClosure(ctx)
	require.NoError(t, err)
	flow.hub = &Hub{}
	for _, unique := range append(uniques(required), "lastlogin-go/backend") {
		flow.hub.add(&phaseRecorder{recordingManager: recordingManager{unique: unique}})
	}
	flow.playbook = &Playbook{}
	flow.policy = &RuntimeStartPolicy{dependencies: flow.world.Dependencies, ExecutorManager: flow}
	failed := errors.New("stop unavailable")
	var failedManager *phaseRecorder
	flow.spawn = func(_ context.Context, _ *resources.Module, service *resources.Service) (IManager, error) {
		manager := &phaseRecorder{recordingManager: recordingManager{unique: resources.WithUnique(service).Unique()}}
		if manager.unique == "wiki/backend" {
			manager.stopErr = failed
			failedManager = manager
		}
		return manager, nil
	}
	flow.WithRunningHook(func(string, int) {})
	reloaded, err := resources.LoadWorkspaceFromDir(ctx, "testdata/multi-root")
	require.NoError(t, err)
	_, err = flow.Extend(ctx, reloaded, []string{"wiki/backend"})
	require.NoError(t, err)
	_, err = flow.Withdraw(ctx, reloaded, []string{"wiki/backend"})
	require.ErrorIs(t, err, failed)
	require.Contains(t, flow.Roots(), "wiki/backend")
	// A subsequent call still sees the root as pending and reaches the stop
	// path instead of returning "nothing to withdraw".
	require.NotNil(t, failedManager)
	failedManager.stopErr = nil
	_, err = flow.Withdraw(ctx, reloaded, []string{"wiki/backend"})
	require.NoError(t, err)
	require.NotContains(t, flow.Roots(), "wiki/backend")
}
