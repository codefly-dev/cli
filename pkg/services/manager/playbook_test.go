package manager_test

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/pkg/services/manager"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/assert"
)

func TestPlaybookRunNoDependencies(t *testing.T) {
	ctx := context.Background()
	wool.SetGlobalLogLevel(wool.DEBUG)
	data := setup(t, execOnInit())
	// "Create"

	start := "billing/no_dependencies"

	playbook, err := manager.NewPlaybook(ctx, data.dependencies)

	playbook.WithPolicy(data.policy)

	// Stop after run
	playbook.WithStopping(func(ctx context.Context, action manager.Action) bool {
		return action.Type == manager.RuntimeStart
	})

	// Run
	err = playbook.Start(ctx, manager.Action{Type: manager.RuntimeCreate, Service: start})
	assert.NoError(t, err)

	expected := createActions(start, manager.RuntimeLoad, manager.RuntimeInit, manager.RuntimeStart)
	assert.Equal(t, expected, unwrap(playbook.Executed()))
}

func TestPlaybookRunNoDependenciesWithSignaller(t *testing.T) {
	ctx := context.Background()
	wool.SetGlobalLogLevel(wool.DEBUG)
	data := setup(t, execOnInit())
	// "Create"

	start := "billing/no_dependencies"

	err := data.policy.Restrict(ctx, start)
	assert.NoError(t, err)

	playbook, err := manager.NewPlaybook(ctx, data.dependencies)

	playbook.WithPolicy(data.policy)

	playbook.WithSignallerFunc(func(_ *manager.Playbook, action manager.Action) *manager.Signal {
		if action.Type == manager.RuntimeStart {
			return &manager.Signal{}
		}
		return nil
	})

	// Run
	go func() {
		// We don't stop
		_ = playbook.Start(ctx, manager.Action{Type: manager.RuntimeCreate, Service: start})
	}()

	// Block on signal
	for {
		select {
		case <-playbook.Signals():
			expected := createActions(start, manager.RuntimeLoad, manager.RuntimeInit, manager.RuntimeStart)
			assert.Equal(t, expected, unwrap(playbook.Executed()))
			return
		}
	}
}

func TestPlaybookPlaybookRunOneDependency(t *testing.T) {
	ctx := context.Background()
	data := setup(t, execOnInit())
	// "Create"

	start := "billing/accounts"
	org := "management/organization"

	err := data.policy.Restrict(ctx, start)
	assert.NoError(t, err)

	actions, err := data.policy.Execute(ctx, manager.Action{Type: manager.RuntimeCreate, Service: start})
	assert.NoError(t, err)
	// Org first then accounts
	assert.Equal(t, createCombinedActions([]string{org, start}, manager.RuntimeLoad), actions, "Expected no action to be triggered")

	playbook, err := manager.NewPlaybook(ctx, data.dependencies)
	playbook.WithPolicy(data.policy)

	// Stop after run
	playbook.WithStopping(func(ctx context.Context, action manager.Action) bool {
		return action.Type == manager.RuntimeStart && action.Service == start
	})

	// Run
	err = playbook.Start(ctx, manager.Action{Type: manager.RuntimeCreate, Service: start})
	assert.NoError(t, err)
	expected := createCombinedActions([]string{org, start}, manager.RuntimeLoad, manager.RuntimeInit, manager.RuntimeStart)
	assert.Equal(t, expected, unwrap(playbook.Executed()))

}

func TestPlaybookRunMoreDependencies(t *testing.T) {
	ctx := context.Background()
	data := setup(t, execOnInit())
	// "Create"

	start := "web/gateway"
	accounts := "billing/accounts"
	org := "management/organization"

	err := data.policy.Restrict(ctx, start)
	assert.NoError(t, err)

	actions, err := data.policy.Execute(ctx, manager.Action{Type: manager.RuntimeCreate, Service: start})
	assert.NoError(t, err)
	// Org first then accounts
	assert.Equal(t, createCombinedActions([]string{org, accounts, start}, manager.RuntimeLoad), actions, "Expected no action to be triggered")

	{
		playbook, err := manager.NewPlaybook(ctx, data.dependencies)
		playbook.WithPolicy(data.policy)

		playbook.WithStopping(func(ctx context.Context, action manager.Action) bool {
			return action.Type == manager.RuntimeLoad && action.Service == start
		})

		err = playbook.Start(ctx, manager.Action{Type: manager.RuntimeCreate, Service: start})
		assert.NoError(t, err)
		expected := createCombinedActions([]string{org, accounts, start}, manager.RuntimeLoad)
		assert.Equal(t, expected, unwrap(playbook.Executed()))
	}

	{
		playbook, err := manager.NewPlaybook(ctx, data.dependencies)
		playbook.WithPolicy(data.policy)

		playbook.WithStopping(func(ctx context.Context, action manager.Action) bool {
			return action.Type == manager.RuntimeInit && action.Service == start
		})

		err = playbook.Start(ctx, manager.Action{Type: manager.RuntimeCreate, Service: start})
		assert.NoError(t, err)
		expected := createCombinedActions([]string{org, accounts, start}, manager.RuntimeLoad, manager.RuntimeInit)
		assert.Equal(t, expected, unwrap(playbook.Executed()))
	}

	{
		playbook, err := manager.NewPlaybook(ctx, data.dependencies)
		playbook.WithPolicy(data.policy)

		playbook.WithStopping(func(ctx context.Context, action manager.Action) bool {
			return action.Type == manager.RuntimeStart && action.Service == start
		})

		err = playbook.Start(ctx, manager.Action{Type: manager.RuntimeCreate, Service: start})
		assert.NoError(t, err)
		expected := createCombinedActions([]string{org, accounts, start}, manager.RuntimeLoad, manager.RuntimeInit, manager.RuntimeStart)
		assert.Equal(t, expected, unwrap(playbook.Executed()))
	}

}

func TestPlaybookRunNoDependenciesWithRestart(t *testing.T) {
	ctx := context.Background()
	wool.SetGlobalLogLevel(wool.DEBUG)
	data := setup(t, execOnInitThenUpdated())

	start := "billing/no_dependencies"

	err := data.policy.Restrict(ctx, start)
	assert.NoError(t, err)

	playbook, err := manager.NewPlaybook(ctx, data.dependencies)

	playbook.WithPolicy(data.policy)
	playbook.WithSignallerFunc(func(_ *manager.Playbook, action manager.Action) *manager.Signal {
		if action.Type == manager.RuntimeStart {
			return &manager.Signal{}
		}
		return nil
	})

	// Run
	stopped := make(chan error)
	go func() {
		// We don't stop YET
		stopped <- playbook.Start(ctx, manager.Action{Type: manager.RuntimeCreate, Service: start})
	}()

	expected := createActions(start, manager.RuntimeLoad, manager.RuntimeInit, manager.RuntimeStart)

	// Block on signal
	for {
		select {
		case <-playbook.Signals():
			assert.Equal(t, expected, unwrap(playbook.Executed()))
			goto signalled
		}
	}
signalled:
	// Now we will send a new action
	playbook.WithStopping(func(ctx context.Context, action manager.Action) bool {
		return action.Type == manager.RuntimeStart
	})

	playbook.ActionManager().Send(ctx, manager.Action{Type: manager.RuntimeInit, Service: start})
	expected = append(expected, createCombinedActions([]string{start}, manager.RuntimeInit, manager.RuntimeStart)...)

	<-stopped
	assert.Equal(t, expected, unwrap(playbook.Executed()))
}
