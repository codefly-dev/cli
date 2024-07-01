package manager_test

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/services/manager"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
)

func TestPlaybookRunNoDependencies(t *testing.T) {
	ctx := context.Background()
	wool.SetGlobalLogLevel(wool.DEBUG)
	data := setup(t, manager.RuntimeStart, execOnInit())

	start := "management/organization"

	playbook, err := manager.NewPlaybook(ctx, data.world)

	require.NoError(t, err)

	playbook.WithPolicy(data.policy)

	// StopIfNeeded after run
	playbook.WithStoppingAfter(func(ctx context.Context, action manager.Action) bool {
		return action.Type == manager.RuntimeStart
	})

	// Run
	err = playbook.Begin(ctx, manager.Action{Type: manager.RuntimeBegin, Service: start})
	require.NoError(t, err)

	expected := createActionsWithRound(1, start, manager.RuntimeLoad, manager.RuntimeInit, manager.RuntimeStart)
	require.Equal(t, expected, playbook.Executed())

}

func TestPlaybookRunNoDependenciesWithSignaller(t *testing.T) {
	ctx := context.Background()
	wool.SetGlobalLogLevel(wool.DEBUG)
	data := setup(t, manager.RuntimeStart, execOnInit())

	start := "management/organization"

	err := data.policy.Restrict(ctx, start)
	require.NoError(t, err)

	playbook, err := manager.NewPlaybook(ctx, data.world)
	require.NoError(t, err)

	playbook.WithPolicy(data.policy)

	playbook.WithSignallerFunc(func(_ *manager.Playbook, action manager.Action) *manager.Signal {
		if action.Type == manager.RuntimeStart {
			return &manager.Signal{}
		}
		return nil
	})

	// Run
	go func() {
		// We don't stopAfter
		_ = playbook.Begin(ctx, manager.Action{Type: manager.RuntimeBegin, Service: start})
	}()

	// Block on signal
	for {
		select {
		case <-playbook.Signals():
			expected := createActionsWithRound(1, start, manager.RuntimeLoad, manager.RuntimeInit, manager.RuntimeStart)
			require.Equal(t, expected, playbook.Executed())
			return
		}
	}
}

func TestPlaybookPlaybookRunOneDependency(t *testing.T) {
	ctx := context.Background()
	data := setup(t, manager.RuntimeStart, execOnInit())

	start := "billing/accounts"
	org := "management/organization"

	err := data.policy.Restrict(ctx, start)
	require.NoError(t, err)

	playbook, err := manager.NewPlaybook(ctx, data.world)

	require.NoError(t, err)

	playbook.WithPolicy(data.policy)

	// StopIfNeeded after run
	playbook.WithStoppingAfter(func(ctx context.Context, action manager.Action) bool {
		return action.Type == manager.RuntimeStart && action.Service == start
	})

	// Run
	err = playbook.Begin(ctx, manager.Action{Type: manager.RuntimeBegin, Service: start})
	require.NoError(t, err)

	expected := createCombinedActionsWithRound(1, []string{org, start}, manager.RuntimeLoad, manager.RuntimeInit, manager.RuntimeStart)
	require.Equal(t, expected, playbook.Executed())

}

func TestPlaybookRunTwoDependencies(t *testing.T) {
	ctx := context.Background()
	data := setup(t, manager.RuntimeStart, execOnInit())

	start := "web/gateway"
	accounts := "billing/accounts"
	org := "management/organization"

	{
		playbook, err := manager.NewPlaybook(ctx, data.world)

		require.NoError(t, err)

		playbook.WithPolicy(data.policy)

		playbook.WithStoppingAfter(func(ctx context.Context, action manager.Action) bool {
			return action.Type == manager.RuntimeLoad && action.Service == start
		})

		err = playbook.Begin(ctx, manager.Action{Type: manager.RuntimeBegin, Service: start})
		require.NoError(t, err)
		expected := createCombinedActionsWithRound(1, []string{org, accounts, start}, manager.RuntimeLoad)
		require.Equal(t, expected, playbook.Executed())
	}

	{
		playbook, err := manager.NewPlaybook(ctx, data.world)
		require.NoError(t, err)

		playbook.WithPolicy(data.policy)

		playbook.WithStoppingAfter(func(ctx context.Context, action manager.Action) bool {
			return action.Type == manager.RuntimeInit && action.Service == start
		})

		err = playbook.Begin(ctx, manager.Action{Type: manager.RuntimeBegin, Service: start})
		require.NoError(t, err)
		expected := createCombinedActionsWithRound(1, []string{org, accounts, start}, manager.RuntimeLoad, manager.RuntimeInit)
		require.Equal(t, expected, playbook.Executed())
	}

	{
		playbook, err := manager.NewPlaybook(ctx, data.world)
		require.NoError(t, err)
		playbook.WithPolicy(data.policy)

		playbook.WithStoppingAfter(func(ctx context.Context, action manager.Action) bool {
			return action.Type == manager.RuntimeStart && action.Service == start
		})

		err = playbook.Begin(ctx, manager.Action{Type: manager.RuntimeBegin, Service: start})
		require.NoError(t, err)
		expected := createCombinedActionsWithRound(1, []string{org, accounts, start}, manager.RuntimeLoad, manager.RuntimeInit, manager.RuntimeStart)
		require.Equal(t, expected, playbook.Executed())
	}

}

func TestPlaybookRunNoDependenciesWithRestart(t *testing.T) {
	ctx, done := common.NewContext()
	defer done()
	wool.SetGlobalLogLevel(wool.DEBUG)
	data := setup(t, manager.RuntimeStart, execOnInitThenUpdated())

	start := "management/organization"

	playbook, err := manager.NewPlaybook(ctx, data.world)
	require.NoError(t, err)

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
		// We don't stopAfter YET
		stopped <- playbook.Begin(ctx, manager.Action{Type: manager.RuntimeBegin, Service: start})
	}()

	expected := createActionsWithRound(1, start, manager.RuntimeLoad, manager.RuntimeInit, manager.RuntimeStart)

	// Block on signal
	for {
		select {
		case <-playbook.Signals():
			goto signalled
		}
	}
signalled:
	require.Equal(t, expected, playbook.Executed())

	// Now we will send a new action
	playbook.WithStoppingAfter(func(ctx context.Context, action manager.Action) bool {
		return action.Type == manager.RuntimeStart
	})

	err = playbook.Seed(ctx, manager.Action{Type: manager.RuntimeInit, Service: start})
	require.NoError(t, err)
	expected = append(expected, createActionsWithRound(2, start, manager.RuntimeInit, manager.RuntimeStart)...)

	<-stopped
	require.Equal(t, expected, playbook.Executed())
}

func TestPlaybookRunOneDependencyWithRestartNoPropagation(t *testing.T) {
	ctx := context.Background()
	wool.SetGlobalLogLevel(wool.DEBUG)
	data := setup(t, manager.RuntimeStart, execOnInitThenUpdated())

	start := "billing/accounts"
	org := "management/organization"

	err := data.policy.Restrict(ctx, start)
	require.NoError(t, err)

	playbook, err := manager.NewPlaybook(ctx, data.world)
	require.NoError(t, err)

	playbook.WithPolicy(data.policy)

	playbook.WithSignallerFunc(func(_ *manager.Playbook, action manager.Action) *manager.Signal {
		if action.Type == manager.RuntimeStart && action.Service == start {
			return &manager.Signal{}
		}
		return nil
	})

	// Run
	stopped := make(chan error)
	go func() {
		// We don't stopAfter YET
		stopped <- playbook.Begin(ctx, manager.Action{Type: manager.RuntimeBegin, Service: start})
	}()

	expected := createCombinedActionsWithRound(1, []string{org, start}, manager.RuntimeLoad, manager.RuntimeInit, manager.RuntimeStart)

	// Block on signal
	for {
		select {
		case <-playbook.Signals():
			goto signalled
		}
	}
signalled:
	executed := playbook.Executed()
	require.Equal(t, expected, executed)

	// Now we will send a new action from the dependency of start
	playbook.WithStoppingAfter(func(ctx context.Context, action manager.Action) bool {
		return action.Type == manager.RuntimeStart && action.Service == org
	})

	// Re-init the "root": organization
	// We shouldn't have any billing (start)

	err = playbook.Seed(ctx, manager.Action{Type: manager.RuntimeInit, Service: org})
	require.NoError(t, err)
	<-stopped

	// New ones
	executed = playbook.Executed()[len(executed):]
	expected = createActionsWithRound(2, org, manager.RuntimeInit, manager.RuntimeStart)

	require.Equal(t, expected, executed)
}

func TestPlaybookRunOneDependencyWithRestartWithPropagation(t *testing.T) {
	ctx, done := common.NewContext()
	defer done()

	wool.SetGlobalLogLevel(wool.DEBUG)
	data := setup(t, manager.RuntimeStart, execOnInitThenPropagate())

	start := "billing/accounts"
	org := "management/organization"

	err := data.policy.Restrict(ctx, start)
	require.NoError(t, err)

	playbook, err := manager.NewPlaybook(ctx, data.world)
	require.NoError(t, err)

	playbook.WithPolicy(data.policy)

	playbook.WithSignallerFunc(func(_ *manager.Playbook, action manager.Action) *manager.Signal {
		if action.Type == manager.RuntimeStart && action.Service == start {
			return &manager.Signal{}
		}
		return nil
	})

	// Run
	stopped := make(chan error)
	go func() {
		// We don't stopAfter YET
		stopped <- playbook.Begin(ctx, manager.Action{Type: manager.RuntimeBegin, Service: start})
	}()

	// Block on signal
	for {
		select {
		case <-playbook.Signals():
			goto signalled
		}
	}
signalled:
	expected := createCombinedActionsWithRound(1, []string{org, start}, manager.RuntimeLoad, manager.RuntimeInit, manager.RuntimeStart)
	executed := playbook.Executed()
	require.Equal(t, expected, executed)

	// Now we will send a new action from the dependency of start
	playbook.WithStoppingAfter(func(ctx context.Context, action manager.Action) bool {
		return action.Type == manager.RuntimeStart && action.Service == start
	})
	// Re-init the "root": organization
	// We shouldn't have any billing (start)

	err = playbook.Seed(ctx, manager.Action{Type: manager.RuntimeInit, Service: org})
	require.NoError(t, err)

	<-stopped
	// New ones
	executed = playbook.Executed()[len(executed):]
	expected = createCombinedActionsWithRound(2, []string{org, start}, manager.RuntimeInit, manager.RuntimeStart)
	require.Equal(t, expected, executed)
}

func TestPlaybookRunTwoDependenciesWithRestartWithPropagation(t *testing.T) {
	ctx, done := common.NewContext()
	defer done()

	wool.SetGlobalLogLevel(wool.DEBUG)
	data := setup(t, manager.RuntimeStart, execOnInitThenPropagate())

	start := "web/gateway"
	accounts := "billing/accounts"
	org := "management/organization"

	playbook, err := manager.NewPlaybook(ctx, data.world)
	require.NoError(t, err)

	playbook.WithPolicy(data.policy)

	playbook.WithSignallerFunc(func(_ *manager.Playbook, action manager.Action) *manager.Signal {
		if action.Type == manager.RuntimeStart && action.Service == start {
			return &manager.Signal{}
		}
		return nil
	})

	// Run
	stopped := make(chan error)
	go func() {
		// We don't stopAfter YET
		stopped <- playbook.Begin(ctx, manager.Action{Type: manager.RuntimeBegin, Service: start})
	}()

	// Block on signal
	for {
		select {
		case <-playbook.Signals():
			goto signalled
		}
	}
signalled:
	expected := createCombinedActionsWithRound(1, []string{org, accounts, start}, manager.RuntimeLoad, manager.RuntimeInit, manager.RuntimeStart)
	executed := playbook.Executed()
	require.Equal(t, expected, executed)

	// Now we will send a new action from the dependency of start
	playbook.WithStoppingAfter(func(ctx context.Context, action manager.Action) bool {
		return action.Type == manager.RuntimeStart && action.Service == start
	})

	err = playbook.Seed(ctx, manager.Action{Type: manager.RuntimeInit, Service: org})
	require.NoError(t, err)

	<-stopped
	// New ones
	executed = playbook.Executed()[len(executed):]
	// We propagate all

	expected = createCombinedActionsWithRound(2, []string{org, accounts, start}, manager.RuntimeInit, manager.RuntimeStart)
	require.Equal(t, expected, executed)
}

func TestPlaybookRunTwoDependenciesWithRestartWithPropagationInMiddleOnly(t *testing.T) {
	ctx, done := common.NewContext()
	defer done()

	start := "web/gateway"
	accounts := "billing/accounts"
	org := "management/organization"

	wool.SetGlobalLogLevel(wool.DEBUG)

	exec := execOnInitThenPropagate()

	// We will propagate only for org after Init
	exec.onlyService = org

	data := setup(t, manager.RuntimeStart, exec)

	playbook, err := manager.NewPlaybook(ctx, data.world)
	require.NoError(t, err)

	playbook.WithPolicy(data.policy)

	playbook.WithSignallerFunc(func(_ *manager.Playbook, action manager.Action) *manager.Signal {
		if action.Type == manager.RuntimeStart && action.Service == start {
			return &manager.Signal{}
		}
		return nil
	})

	// Run
	stopped := make(chan error)
	go func() {
		// We don't stopAfter YET
		stopped <- playbook.Begin(ctx, manager.Action{Type: manager.RuntimeBegin, Service: start})
	}()

	// Block on signal
	for {
		select {
		case <-playbook.Signals():
			goto signalled
		}
	}
signalled:
	expected := createCombinedActionsWithRound(1, []string{org, accounts, start}, manager.RuntimeLoad, manager.RuntimeInit, manager.RuntimeStart)
	executed := playbook.Executed()
	require.Equal(t, expected, executed)

	// Now we will send a new action from the dependency of start
	playbook.WithStoppingAfter(func(ctx context.Context, action manager.Action) bool {
		return action.Type == manager.RuntimeStart && action.Service == accounts // We don't propagate all the way
	})

	// Re-init the "root": organization
	// Only org propagate so we shouldn't get any gateway
	err = playbook.Seed(ctx, manager.Action{Type: manager.RuntimeInit, Service: org})
	require.NoError(t, err)

	<-stopped
	// New ones
	executed = playbook.Executed()[len(executed):]

	expected = createCombinedActionsWithRound(2, []string{org, accounts}, manager.RuntimeInit, manager.RuntimeStart)
	require.Equal(t, expected, executed)
}

type onExecFailsOn struct {
	unique     string
	workingNow bool
	stage      manager.ActionType
}

func (o *onExecFailsOn) GetExecutor(ctx context.Context, action manager.Action) (manager.OutputProcessorFunc, error) {
	return func(ctx context.Context) (*manager.OutputProperty, error) {
		if action.Service == o.unique && action.Type == o.stage {
			if !o.workingNow {
				o.workingNow = true
				return manager.Pause(), nil
			} else {
				return manager.RequirePropagation(), nil
			}
		}
		return manager.OnInit(), nil
	}, nil
}

func execFailFirst(unique string, stage manager.ActionType) *onExecFailsOn {
	return &onExecFailsOn{
		unique: unique,
		stage:  stage,
	}
}

func TestErrorOnLoadNoDependencies(t *testing.T) {
	ctx := context.Background()
	wool.SetGlobalLogLevel(wool.DEBUG)
	start := "management/organization"

	data := setup(t, manager.RuntimeStart, execFailFirst(start, manager.RuntimeLoad))

	playbook, err := manager.NewPlaybook(ctx, data.world)
	require.NoError(t, err)

	playbook.WithPolicy(data.policy)

	playbook.WithSignallerFunc(func(_ *manager.Playbook, action manager.Action) *manager.Signal {
		if action.Failed {
			return &manager.Signal{}
		}
		return nil
	})

	// Run
	stopped := make(chan error)
	go func() {
		// We don't stopAfter YET
		stopped <- playbook.Begin(ctx, manager.Action{Type: manager.RuntimeBegin, Service: start})
	}()

	// Block on signal
	for {
		select {
		case <-playbook.Signals():
			goto signalled
		}
	}
signalled:
	expected := createCombinedActionsWithRound(1, []string{start}, manager.RuntimeLoad)
	executed := playbook.Executed()
	require.Equal(t, expected, executed)

	// Now we will send a new action from the dependency of start
	playbook.WithStoppingAfter(func(ctx context.Context, action manager.Action) bool {
		return action.Type == manager.RuntimeStart && action.Service == start
	})

	err = playbook.Seed(ctx, manager.Action{Type: manager.RuntimeLoad, Service: start})
	require.NoError(t, err)

	<-stopped
	// New ones
	executed = playbook.Executed()[len(executed):]
	// We don't propagate to start
	expected = createCombinedActionsWithRound(2, []string{start}, manager.RuntimeLoad, manager.RuntimeInit, manager.RuntimeStart)
	require.Equal(t, expected, executed)

}

func TestErrorOnLoadOneDependency(t *testing.T) {
	ctx := context.Background()
	wool.SetGlobalLogLevel(wool.DEBUG)

	start := "billing/accounts"
	org := "management/organization"

	data := setup(t, manager.RuntimeStart, execFailFirst(org, manager.RuntimeInit))

	playbook, err := manager.NewPlaybook(ctx, data.world)
	require.NoError(t, err)
	playbook.WithPolicy(data.policy)

	playbook.WithSignallerFunc(func(_ *manager.Playbook, action manager.Action) *manager.Signal {
		if action.Failed {
			return &manager.Signal{}
		}
		return nil
	})

	// Run
	stopped := make(chan error)
	go func() {
		// We don't stopAfter YET
		stopped <- playbook.Begin(ctx, manager.Action{Type: manager.RuntimeBegin, Service: start})
	}()

	// Block on signal
	for {
		select {
		case <-playbook.Signals():
			goto signalled
		}
	}
signalled:
	expected := createCombinedActionsWithRound(1, []string{org, start}, manager.RuntimeLoad)
	// Fail at org
	expected = append(expected, createActionsWithRound(1, org, manager.RuntimeInit)...)
	executed := playbook.Executed()
	require.Equal(t, expected, executed)

	// Now we will send a new action from the dependency of start
	playbook.WithStoppingAfter(func(ctx context.Context, action manager.Action) bool {
		return action.Type == manager.RuntimeStart && action.Service == start
	})

	err = playbook.Seed(ctx, manager.Action{Type: manager.RuntimeInit, Service: org})
	require.NoError(t, err)

	<-stopped
	// New ones
	executed = playbook.Executed()[len(executed):]
	expected = createCombinedActionsWithRound(2, []string{org, start}, manager.RuntimeInit, manager.RuntimeStart)
	require.Equal(t, expected, executed)

}
