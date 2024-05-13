package manager_test

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/cli/pkg/services/manager"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
)

type onInit struct {
}

func execOnInit() *onInit {
	return &onInit{}
}

func (a *onInit) GetExecutor(ctx context.Context, action manager.Action) (manager.OutputProcessorFunc, error) {
	return func(ctx context.Context) (*manager.OutputProperty, error) {
		return manager.OnInit(), nil
	}, nil
}

// Pause: just once or panic
type onExecWait struct {
	loaded bool
}

func (o *onExecWait) GetExecutor(ctx context.Context, action manager.Action) (manager.OutputProcessorFunc, error) {
	return func(ctx context.Context) (*manager.OutputProperty, error) {
		if !o.loaded {
			o.loaded = true
			return manager.Pause(), nil
		}
		panic("should not be called")

	}, nil
}

func execWait() *onExecWait {
	return &onExecWait{}
}

type initThenUpdated struct {
	onInit bool
}

func execOnInitThenUpdated() *initThenUpdated {
	return &initThenUpdated{onInit: true}
}

func (a *initThenUpdated) GetExecutor(ctx context.Context, action manager.Action) (manager.OutputProcessorFunc, error) {
	return func(ctx context.Context) (*manager.OutputProperty, error) {
		if a.onInit {
			a.onInit = false
			return manager.OnInit(), nil
		}
		return manager.IndependentUpdate(), nil
	}, nil
}

var _ manager.ExecutorManager = &initThenUpdated{}

type initThenPropagate struct {
	onInit      bool
	onlyService string
}

func execOnInitThenPropagate() *initThenPropagate {
	return &initThenPropagate{onInit: true}
}

func (a *initThenPropagate) GetExecutor(ctx context.Context, action manager.Action) (manager.OutputProcessorFunc, error) {
	return func(ctx context.Context) (*manager.OutputProperty, error) {
		if a.onInit {
			a.onInit = false
			return manager.OnInit(), nil
		}
		if a.onlyService == "" {
			return manager.RequirePropagation(), nil
		}
		if action.Service == a.onlyService {
			return manager.RequirePropagation(), nil
		}
		return manager.IndependentUpdate(), nil
	}, nil
}

var _ manager.ExecutorManager = &initThenPropagate{}

type setupData struct {
	workspace *resources.Workspace
	world     *manager.World
	policy    manager.PlaybookPolicy
}

func setup(t *testing.T, actionType manager.ActionType, executor manager.ExecutorManager) setupData {
	wool.SetGlobalLogLevel(wool.DEBUG)
	// modules:
	// management:
	// - organization
	// billing
	// - accounts -> management/organization
	// web:
	// - frontend -> gateway
	// - gateway  -> billing/accounts

	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, "testdata/module-layout")
	require.NoError(t, err)

	dependencies, err := architecture.NewServiceDependencies(ctx, workspace)
	require.NoError(t, err)

	w := wool.Get(ctx).In("setup")
	w.Debug(dependencies.Print())

	var policy manager.PlaybookPolicy
	switch actionType {
	case manager.RuntimeStart:
		runtimeStartPolicy, err := manager.NewRuntimeStartPolicy(ctx, dependencies, executor)
		require.NoError(t, err)
		policy = runtimeStartPolicy
	case manager.BuilderSync:
		syncPolicy, err := manager.NewSyncPolicy(ctx, dependencies, executor)
		require.NoError(t, err)
		policy = syncPolicy
	}

	world := &manager.World{
		Dependencies: dependencies,
	}

	return setupData{
		workspace: workspace,
		world:     world,
		policy:    policy,
	}
}

func createActions(service string, types ...manager.ActionType) []manager.Action {
	var actions []manager.Action
	for _, action := range types {
		actions = append(actions, manager.Action{Type: action, Service: service})
	}
	return actions
}

func createActionsWithRound(round int, service string, types ...manager.ActionType) []manager.Action {
	var actions []manager.Action
	for _, action := range types {
		actions = append(actions, manager.Action{Type: action, Service: service, Round: round})
	}
	return actions
}

func createCombinedActions(services []string, types ...manager.ActionType) []manager.Action {
	var actions []manager.Action
	for _, action := range types {
		for _, service := range services {
			actions = append(actions, manager.Action{Type: action, Service: service})
		}
	}
	return actions
}

func createCombinedActionsWithRound(round int, services []string, types ...manager.ActionType) []manager.Action {
	var actions []manager.Action
	for _, action := range types {
		for _, service := range services {
			actions = append(actions, manager.Action{Type: action, Service: service, Round: round})
		}
	}
	return actions
}

func TestRunPolicyNoDependencies(t *testing.T) {
	ctx := context.Background()
	data := setup(t, manager.RuntimeStart, execOnInit())
	// "Create"

	start := "billing/no_dependencies"

	err := data.policy.Restrict(ctx, start)
	require.NoError(t, err)

	actions, err := data.policy.Execute(ctx, manager.Action{Type: manager.RuntimeBegin, Service: start})
	require.NoError(t, err)
	require.Equal(t, createActions(start, manager.RuntimeLoad), actions, "Expected no action to be triggered")

	actions, err = data.policy.Execute(ctx, manager.Action{Type: manager.RuntimeLoad, Service: start})
	require.NoError(t, err)
	require.Equal(t, createActions(start, manager.RuntimeInit), actions, "Expected no action to be triggered")

	actions, err = data.policy.Execute(ctx, manager.Action{Type: manager.RuntimeInit, Service: start})
	require.NoError(t, err)
	require.Equal(t, createActions(start, manager.RuntimeStart), actions, "Expected no action to be triggered")

	actions, err = data.policy.Execute(ctx, manager.Action{Type: manager.RuntimeStart, Service: start})
	require.NoError(t, err)
	require.Equal(t, createActions(start), actions, "We are done")
}

func TestRunPolicyOneDependency(t *testing.T) {
	ctx := context.Background()
	data := setup(t, manager.RuntimeStart, execOnInit())
	// "Create"

	start := "billing/accounts"
	org := "management/organization"

	err := data.policy.Restrict(ctx, start)
	require.NoError(t, err)

	actions, err := data.policy.Execute(ctx, manager.Action{Type: manager.RuntimeBegin, Service: start})
	require.NoError(t, err)
	require.Equal(t, createCombinedActions([]string{org, start}, manager.RuntimeLoad), actions, "Expected no action to be triggered")
}

func TestRunPolicyNoDependencySimulateError(t *testing.T) {
	ctx := context.Background()
	data := setup(t, manager.RuntimeStart, execWait())
	// "Create"

	start := "billing/accounts"

	err := data.policy.Restrict(ctx, start)
	require.NoError(t, err)

	actions, err := data.policy.Execute(ctx, manager.Action{Type: manager.RuntimeLoad, Service: start})
	require.NoError(t, err)
	require.Equal(t, 1, len(actions))
	require.True(t, actions[0].Failed)

}

func TestRunPolicyOneDependencySimulateError(t *testing.T) {
	ctx := context.Background()
	data := setup(t, manager.RuntimeStart, execWait())
	// "Create"

	start := "billing/accounts"
	org := "management/organization"

	err := data.policy.Restrict(ctx, start)
	require.NoError(t, err)

	actions, err := data.policy.Execute(ctx, manager.Action{Type: manager.RuntimeLoad, Service: org})
	require.NoError(t, err)
	require.Equal(t, 1, len(actions))
	require.True(t, actions[0].Failed)
}
