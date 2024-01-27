package manager_test

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/cli/pkg/services/manager"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/assert"
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

type initThenUpdated struct {
	onInit bool
}

func execOnInitThenUpdated() *initThenUpdated {
	return &initThenUpdated{}
}

func (a *initThenUpdated) GetExecutor(ctx context.Context, action manager.Action) (manager.OutputProcessorFunc, error) {
	return func(ctx context.Context) (*manager.OutputProperty, error) {
		return manager.OnInit(), nil
	}, nil
}

var _ manager.ExecutorManager = &initThenUpdated{}

type setupData struct {
	project      *configurations.Project
	dependencies *architecture.ServiceDependencies
	policy       *manager.RuntimeStartPolicy
}

func setup(t *testing.T, executor manager.ExecutorManager) setupData {
	wool.SetGlobalLogLevel(wool.DEBUG)
	// applications:
	// management:
	// - organization
	// billing
	// - accounts -> management/organization
	// web:
	// - frontend -> gateway
	// - gateway  -> management/organization
	// - gateway  -> billing/accounts

	ctx := context.Background()
	project, err := configurations.LoadProjectFromDirUnsafe(ctx, "testdata/codefly-platform")
	assert.NoError(t, err)

	dependencies, err := architecture.NewServiceDependencies(ctx, project)
	assert.NoError(t, err)

	w := wool.Get(ctx).In("setup")
	w.Debug(dependencies.Print())

	runtimeStartPolicy, err := manager.NewRuntimeStartPolicy(ctx, dependencies, executor)
	assert.NoError(t, err)

	return setupData{
		project:      project,
		dependencies: dependencies,
		policy:       runtimeStartPolicy,
	}
}

func createActions(service string, types ...manager.ActionType) []manager.Action {
	var actions []manager.Action
	for _, action := range types {
		actions = append(actions, manager.Action{Type: action, Service: service})
	}
	return actions
}

func unwrap(actions []manager.Action) []manager.Action {
	var out []manager.Action
	for _, action := range actions {
		out = append(out, manager.Action{Type: action.Type, Service: action.Service})
	}
	return out
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

func TestRunPolicyNoDependencies(t *testing.T) {
	ctx := context.Background()
	data := setup(t, execOnInit())
	// "Create"

	start := "billing/no_dependencies"

	err := data.policy.Restrict(ctx, start)
	assert.NoError(t, err)

	actions, err := data.policy.Execute(ctx, manager.Action{Type: manager.RuntimeCreate, Service: start})
	assert.NoError(t, err)
	assert.Equal(t, createActions(start, manager.RuntimeLoad), unwrap(actions), "Expected no action to be triggered")

	actions, err = data.policy.Execute(ctx, manager.Action{Type: manager.RuntimeLoad, Service: start})
	assert.NoError(t, err)
	assert.Equal(t, createActions(start, manager.RuntimeInit), unwrap(actions), "Expected no action to be triggered")

	actions, err = data.policy.Execute(ctx, manager.Action{Type: manager.RuntimeInit, Service: start})
	assert.NoError(t, err)
	assert.Equal(t, createActions(start, manager.RuntimeStart), unwrap(actions), "Expected no action to be triggered")
}

func TestRunPolicyOneDependency(t *testing.T) {
	ctx := context.Background()
	data := setup(t, execOnInit())
	// "Create"

	start := "billing/accounts"
	org := "management/organization"

	err := data.policy.Restrict(ctx, start)
	assert.NoError(t, err)

	actions, err := data.policy.Execute(ctx, manager.Action{Type: manager.RuntimeCreate, Service: start})
	assert.NoError(t, err)
	assert.Equal(t, createCombinedActions([]string{org, start}, manager.RuntimeLoad), unwrap(actions), "Expected no action to be triggered")
}
