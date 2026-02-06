package orchestration_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/architecture"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/wool"
	"github.com/stretchr/testify/require"
)

type onInit struct {
}

func execOnInit() *onInit {
	return &onInit{}
}

func (a *onInit) GetExecutor(ctx context.Context, action orchestration.Action) (orchestration.OutputProcessorFunc, error) {
	return func(ctx context.Context) (*orchestration.OutputProperty, error) {
		return orchestration.OnInit(), nil
	}, nil
}

// Pause: just once or panic
type onExecWait struct {
	loaded bool
}

func (o *onExecWait) GetExecutor(ctx context.Context, action orchestration.Action) (orchestration.OutputProcessorFunc, error) {
	return func(ctx context.Context) (*orchestration.OutputProperty, error) {
		if !o.loaded {
			o.loaded = true
			return orchestration.Pause(), nil
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

func (a *initThenUpdated) GetExecutor(ctx context.Context, action orchestration.Action) (orchestration.OutputProcessorFunc, error) {
	return func(ctx context.Context) (*orchestration.OutputProperty, error) {
		if a.onInit {
			a.onInit = false
			return orchestration.OnInit(), nil
		}
		return orchestration.IndependentUpdate(), nil
	}, nil
}

var _ orchestration.ExecutorManager = &initThenUpdated{}

type initThenPropagate struct {
	onInit      bool
	onlyService string
}

func execOnInitThenPropagate() *initThenPropagate {
	return &initThenPropagate{onInit: true}
}

func (a *initThenPropagate) GetExecutor(ctx context.Context, action orchestration.Action) (orchestration.OutputProcessorFunc, error) {
	return func(ctx context.Context) (*orchestration.OutputProperty, error) {
		if a.onInit {
			fmt.Println("ON INIT", action)
			a.onInit = false
			return orchestration.OnInit(), nil
		}
		if a.onlyService == "" {
			fmt.Println("PROPAGATE", action)
			return orchestration.RequirePropagation(), nil
		}
		if action.Service == a.onlyService {
			fmt.Println("PROPAGATE ONLY", action)
			return orchestration.RequirePropagation(), nil
		}
		fmt.Println("INDEPENDEMT UPDATE", action)
		return orchestration.IndependentUpdate(), nil
	}, nil
}

var _ orchestration.ExecutorManager = &initThenPropagate{}

type setupData struct {
	workspace *resources.Workspace
	world     *orchestration.World
	policy    orchestration.PlaybookPolicy
}

func setup(t *testing.T, actionType orchestration.ActionType, executor orchestration.ExecutorManager) setupData {
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

	var policy orchestration.PlaybookPolicy
	switch actionType {
	case orchestration.RuntimeStart:
		runtimeStartPolicy, err := orchestration.NewRuntimeStartPolicy(ctx, dependencies, executor)
		require.NoError(t, err)
		policy = runtimeStartPolicy
	case orchestration.BuilderSync:
		syncPolicy, err := orchestration.NewSyncPolicy(ctx, dependencies, executor)
		require.NoError(t, err)
		policy = syncPolicy
	}

	world := &orchestration.World{
		Dependencies: dependencies,
	}

	return setupData{
		workspace: workspace,
		world:     world,
		policy:    policy,
	}
}

func createActions(service string, types ...orchestration.ActionType) []orchestration.Action {
	var actions []orchestration.Action
	for _, action := range types {
		actions = append(actions, orchestration.Action{Type: action, Service: service})
	}
	return actions
}

func createActionsWithRound(round int, service string, types ...orchestration.ActionType) []orchestration.Action {
	var actions []orchestration.Action
	for _, action := range types {
		actions = append(actions, orchestration.Action{Type: action, Service: service, Round: round})
	}
	return actions
}

func createCombinedActions(services []string, types ...orchestration.ActionType) []orchestration.Action {
	var actions []orchestration.Action
	for _, action := range types {
		for _, service := range services {
			actions = append(actions, orchestration.Action{Type: action, Service: service})
		}
	}
	return actions
}

func createCombinedActionsWithRound(round int, services []string, types ...orchestration.ActionType) []orchestration.Action {
	var actions []orchestration.Action
	for _, action := range types {
		for _, service := range services {
			actions = append(actions, orchestration.Action{Type: action, Service: service, Round: round})
		}
	}
	return actions
}

func TestRunPolicyNoDependencies(t *testing.T) {
	ctx := context.Background()
	data := setup(t, orchestration.RuntimeStart, execOnInit())

	start := "management/organization"

	err := data.policy.Restrict(ctx, start)
	require.NoError(t, err)

	actions, err := data.policy.Execute(ctx, orchestration.Action{Type: orchestration.RuntimeBegin, Service: start})
	require.NoError(t, err)
	require.Equal(t, createActions(start, orchestration.RuntimeLoad), actions, "Expected no action to be triggered")

	actions, err = data.policy.Execute(ctx, orchestration.Action{Type: orchestration.RuntimeLoad, Service: start})
	require.NoError(t, err)
	require.Equal(t, createActions(start, orchestration.RuntimeInit), actions, "Expected no action to be triggered")

	actions, err = data.policy.Execute(ctx, orchestration.Action{Type: orchestration.RuntimeInit, Service: start})
	require.NoError(t, err)
	require.Equal(t, createActions(start, orchestration.RuntimeStart), actions, "Expected no action to be triggered")

	actions, err = data.policy.Execute(ctx, orchestration.Action{Type: orchestration.RuntimeStart, Service: start})
	require.NoError(t, err)
	require.Equal(t, createActions(start), actions, "We are done")
}

func TestRunPolicyOneDependency(t *testing.T) {
	ctx := context.Background()
	data := setup(t, orchestration.RuntimeStart, execOnInit())

	start := "billing/accounts"
	org := "management/organization"

	err := data.policy.Restrict(ctx, start)
	require.NoError(t, err)

	actions, err := data.policy.Execute(ctx, orchestration.Action{Type: orchestration.RuntimeBegin, Service: start})
	require.NoError(t, err)
	require.Equal(t, createCombinedActions([]string{org, start}, orchestration.RuntimeLoad), actions, "Expected no action to be triggered")
}

func TestRunPolicyNoDependencySimulateError(t *testing.T) {
	ctx := context.Background()
	data := setup(t, orchestration.RuntimeStart, execWait())

	start := "billing/accounts"

	err := data.policy.Restrict(ctx, start)
	require.NoError(t, err)

	actions, err := data.policy.Execute(ctx, orchestration.Action{Type: orchestration.RuntimeLoad, Service: start})
	require.NoError(t, err)
	require.Equal(t, 1, len(actions))
	require.True(t, actions[0].Failed)

}

func TestRunPolicyOneDependencySimulateError(t *testing.T) {
	ctx := context.Background()
	data := setup(t, orchestration.RuntimeStart, execWait())

	start := "billing/accounts"
	org := "management/organization"

	err := data.policy.Restrict(ctx, start)
	require.NoError(t, err)

	actions, err := data.policy.Execute(ctx, orchestration.Action{Type: orchestration.RuntimeLoad, Service: org})
	require.NoError(t, err)
	require.Equal(t, 1, len(actions))
	require.True(t, actions[0].Failed)
}
