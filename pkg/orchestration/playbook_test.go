package orchestration_test

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/wool"
	"github.com/stretchr/testify/require"
)

func TestPlaybookRunNoDependencies(t *testing.T) {
	ctx := context.Background()
	wool.SetGlobalLogLevel(wool.DEBUG)
	data := setup(t, orchestration.RuntimeStart, execOnInit())

	start := "management/organization"

	playbook, err := orchestration.NewPlaybook(ctx, data.world)

	require.NoError(t, err)

	playbook.WithPolicy(data.policy)

	// StopIfNeeded after run
	playbook.WithStoppingAfter(func(ctx context.Context, action orchestration.Action) bool {
		return action.Type == orchestration.RuntimeStart
	})

	// Run
	err = playbook.Begin(ctx, orchestration.Action{Type: orchestration.RuntimeBegin, Service: start})
	require.NoError(t, err)

	expected := createActionsWithRound(1, start, orchestration.RuntimeLoad, orchestration.RuntimeInit, orchestration.RuntimeStart)
	require.Equal(t, expected, playbook.Executed())

}

func TestPlaybookRunNoDependenciesWithSignaller(t *testing.T) {
	ctx := context.Background()
	wool.SetGlobalLogLevel(wool.DEBUG)
	data := setup(t, orchestration.RuntimeStart, execOnInit())

	start := "management/organization"

	err := data.policy.Restrict(ctx, start)
	require.NoError(t, err)

	playbook, err := orchestration.NewPlaybook(ctx, data.world)
	require.NoError(t, err)

	playbook.WithPolicy(data.policy)

	playbook.WithSignallerFunc(func(_ *orchestration.Playbook, action orchestration.Action) *orchestration.Signal {
		if action.Type == orchestration.RuntimeStart {
			return &orchestration.Signal{}
		}
		return nil
	})

	// Run
	go func() {
		// We don't stopAfter
		_ = playbook.Begin(ctx, orchestration.Action{Type: orchestration.RuntimeBegin, Service: start})
	}()

	// Block on signal
	for {
		select {
		case <-playbook.Signals():
			expected := createActionsWithRound(1, start, orchestration.RuntimeLoad, orchestration.RuntimeInit, orchestration.RuntimeStart)
			require.Equal(t, expected, playbook.Executed())
			return
		}
	}
}

func TestPlaybookPlaybookRunOneDependency(t *testing.T) {
	ctx := context.Background()
	data := setup(t, orchestration.RuntimeStart, execOnInit())

	start := "billing/accounts"
	org := "management/organization"

	err := data.policy.Restrict(ctx, start)
	require.NoError(t, err)

	playbook, err := orchestration.NewPlaybook(ctx, data.world)

	require.NoError(t, err)

	playbook.WithPolicy(data.policy)

	// StopIfNeeded after run
	playbook.WithStoppingAfter(func(ctx context.Context, action orchestration.Action) bool {
		return action.Type == orchestration.RuntimeStart && action.Service == start
	})

	// Run
	err = playbook.Begin(ctx, orchestration.Action{Type: orchestration.RuntimeBegin, Service: start})
	require.NoError(t, err)

	expected := createCombinedActionsWithRound(1, []string{org, start}, orchestration.RuntimeLoad, orchestration.RuntimeInit, orchestration.RuntimeStart)
	require.Equal(t, expected, playbook.Executed())

}

func TestPlaybookRunTwoDependencies(t *testing.T) {
	ctx := context.Background()
	data := setup(t, orchestration.RuntimeStart, execOnInit())

	start := "web/gateway"
	accounts := "billing/accounts"
	org := "management/organization"

	{
		playbook, err := orchestration.NewPlaybook(ctx, data.world)

		require.NoError(t, err)

		playbook.WithPolicy(data.policy)

		playbook.WithStoppingAfter(func(ctx context.Context, action orchestration.Action) bool {
			return action.Type == orchestration.RuntimeLoad && action.Service == start
		})

		err = playbook.Begin(ctx, orchestration.Action{Type: orchestration.RuntimeBegin, Service: start})
		require.NoError(t, err)
		expected := createCombinedActionsWithRound(1, []string{org, accounts, start}, orchestration.RuntimeLoad)
		require.Equal(t, expected, playbook.Executed())
	}

	{
		playbook, err := orchestration.NewPlaybook(ctx, data.world)
		require.NoError(t, err)

		playbook.WithPolicy(data.policy)

		playbook.WithStoppingAfter(func(ctx context.Context, action orchestration.Action) bool {
			return action.Type == orchestration.RuntimeInit && action.Service == start
		})

		err = playbook.Begin(ctx, orchestration.Action{Type: orchestration.RuntimeBegin, Service: start})
		require.NoError(t, err)
		expected := createCombinedActionsWithRound(1, []string{org, accounts, start}, orchestration.RuntimeLoad, orchestration.RuntimeInit)
		require.Equal(t, expected, playbook.Executed())
	}

	{
		playbook, err := orchestration.NewPlaybook(ctx, data.world)
		require.NoError(t, err)
		playbook.WithPolicy(data.policy)

		playbook.WithStoppingAfter(func(ctx context.Context, action orchestration.Action) bool {
			return action.Type == orchestration.RuntimeStart && action.Service == start
		})

		err = playbook.Begin(ctx, orchestration.Action{Type: orchestration.RuntimeBegin, Service: start})
		require.NoError(t, err)
		expected := createCombinedActionsWithRound(1, []string{org, accounts, start}, orchestration.RuntimeLoad, orchestration.RuntimeInit, orchestration.RuntimeStart)
		require.Equal(t, expected, playbook.Executed())
	}

}

func TestPlaybookRunNoDependenciesWithRestart(t *testing.T) {
	ctx, done := common.NewContext()
	defer done()
	wool.SetGlobalLogLevel(wool.DEBUG)
	data := setup(t, orchestration.RuntimeStart, execOnInitThenUpdated())

	start := "management/organization"

	playbook, err := orchestration.NewPlaybook(ctx, data.world)
	require.NoError(t, err)

	playbook.WithPolicy(data.policy)
	playbook.WithSignallerFunc(func(_ *orchestration.Playbook, action orchestration.Action) *orchestration.Signal {
		if action.Type == orchestration.RuntimeStart {
			return &orchestration.Signal{}
		}
		return nil
	})

	// Run
	stopped := make(chan error)
	go func() {
		// We don't stopAfter YET
		stopped <- playbook.Begin(ctx, orchestration.Action{Type: orchestration.RuntimeBegin, Service: start})
	}()

	expected := createActionsWithRound(1, start, orchestration.RuntimeLoad, orchestration.RuntimeInit, orchestration.RuntimeStart)

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
	playbook.WithStoppingAfter(func(ctx context.Context, action orchestration.Action) bool {
		return action.Type == orchestration.RuntimeStart
	})

	err = playbook.Seed(ctx, orchestration.Action{Type: orchestration.RuntimeInit, Service: start})
	require.NoError(t, err)
	expected = append(expected, createActionsWithRound(2, start, orchestration.RuntimeInit, orchestration.RuntimeStart)...)

	<-stopped
	require.Equal(t, expected, playbook.Executed())
}

func TestPlaybookRunOneDependencyWithRestartNoPropagation(t *testing.T) {
	ctx := context.Background()
	wool.SetGlobalLogLevel(wool.DEBUG)
	data := setup(t, orchestration.RuntimeStart, execOnInitThenUpdated())

	start := "billing/accounts"
	org := "management/organization"

	err := data.policy.Restrict(ctx, start)
	require.NoError(t, err)

	playbook, err := orchestration.NewPlaybook(ctx, data.world)
	require.NoError(t, err)

	playbook.WithPolicy(data.policy)

	playbook.WithSignallerFunc(func(_ *orchestration.Playbook, action orchestration.Action) *orchestration.Signal {
		if action.Type == orchestration.RuntimeStart && action.Service == start {
			return &orchestration.Signal{}
		}
		return nil
	})

	// Run
	stopped := make(chan error)
	go func() {
		// We don't stopAfter YET
		stopped <- playbook.Begin(ctx, orchestration.Action{Type: orchestration.RuntimeBegin, Service: start})
	}()

	expected := createCombinedActionsWithRound(1, []string{org, start}, orchestration.RuntimeLoad, orchestration.RuntimeInit, orchestration.RuntimeStart)

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
	playbook.WithStoppingAfter(func(ctx context.Context, action orchestration.Action) bool {
		return action.Type == orchestration.RuntimeStart && action.Service == org
	})

	// Re-init the "root": organization
	// We shouldn't have any billing (start)

	err = playbook.Seed(ctx, orchestration.Action{Type: orchestration.RuntimeInit, Service: org})
	require.NoError(t, err)
	<-stopped

	// New ones
	executed = playbook.Executed()[len(executed):]
	expected = createActionsWithRound(2, org, orchestration.RuntimeInit, orchestration.RuntimeStart)

	require.Equal(t, expected, executed)
}

func TestPlaybookRunOneDependencyWithRestartWithPropagation(t *testing.T) {
	ctx, done := common.NewContext()
	defer done()

	wool.SetGlobalLogLevel(wool.DEBUG)
	data := setup(t, orchestration.RuntimeStart, execOnInitThenPropagate())

	start := "billing/accounts"
	org := "management/organization"

	err := data.policy.Restrict(ctx, start)
	require.NoError(t, err)

	playbook, err := orchestration.NewPlaybook(ctx, data.world)
	require.NoError(t, err)

	playbook.WithPolicy(data.policy)

	playbook.WithSignallerFunc(func(_ *orchestration.Playbook, action orchestration.Action) *orchestration.Signal {
		if action.Type == orchestration.RuntimeStart && action.Service == start {
			return &orchestration.Signal{}
		}
		return nil
	})

	// Run
	stopped := make(chan error)
	go func() {
		// We don't stopAfter YET
		stopped <- playbook.Begin(ctx, orchestration.Action{Type: orchestration.RuntimeBegin, Service: start})
	}()

	// Block on signal
	for {
		select {
		case <-playbook.Signals():
			goto signalled
		}
	}
signalled:
	expected := createCombinedActionsWithRound(1, []string{org, start}, orchestration.RuntimeLoad, orchestration.RuntimeInit, orchestration.RuntimeStart)
	executed := playbook.Executed()
	require.Equal(t, expected, executed)

	// Now we will send a new action from the dependency of start
	playbook.WithStoppingAfter(func(ctx context.Context, action orchestration.Action) bool {
		return action.Type == orchestration.RuntimeStart && action.Service == start
	})
	// Re-init the "root": organization
	// We shouldn't have any billing (start)

	err = playbook.Seed(ctx, orchestration.Action{Type: orchestration.RuntimeInit, Service: org})
	require.NoError(t, err)

	<-stopped
	// New ones
	executed = playbook.Executed()[len(executed):]
	expected = createCombinedActionsWithRound(2, []string{org, start}, orchestration.RuntimeInit, orchestration.RuntimeStart)
	require.Equal(t, expected, executed)
}

func TestPlaybookRunTwoDependenciesWithRestartWithPropagation(t *testing.T) {
	ctx, done := common.NewContext()
	defer done()

	wool.SetGlobalLogLevel(wool.DEBUG)
	data := setup(t, orchestration.RuntimeStart, execOnInitThenPropagate())

	start := "web/gateway"
	accounts := "billing/accounts"
	org := "management/organization"

	playbook, err := orchestration.NewPlaybook(ctx, data.world)
	require.NoError(t, err)

	playbook.WithPolicy(data.policy)

	playbook.WithSignallerFunc(func(_ *orchestration.Playbook, action orchestration.Action) *orchestration.Signal {
		if action.Type == orchestration.RuntimeStart && action.Service == start {
			return &orchestration.Signal{}
		}
		return nil
	})

	// Run
	stopped := make(chan error)
	go func() {
		// We don't stopAfter YET
		stopped <- playbook.Begin(ctx, orchestration.Action{Type: orchestration.RuntimeBegin, Service: start})
	}()

	// Block on signal
	for {
		select {
		case <-playbook.Signals():
			goto signalled
		}
	}
signalled:
	expected := createCombinedActionsWithRound(1, []string{org, accounts, start}, orchestration.RuntimeLoad, orchestration.RuntimeInit, orchestration.RuntimeStart)
	executed := playbook.Executed()
	require.Equal(t, expected, executed)

	// Now we will send a new action from the dependency of start
	playbook.WithStoppingAfter(func(ctx context.Context, action orchestration.Action) bool {
		return action.Type == orchestration.RuntimeStart && action.Service == start
	})

	err = playbook.Seed(ctx, orchestration.Action{Type: orchestration.RuntimeInit, Service: org})
	require.NoError(t, err)

	<-stopped
	// New ones
	executed = playbook.Executed()[len(executed):]
	// We propagate all

	expected = createCombinedActionsWithRound(2, []string{org, accounts, start}, orchestration.RuntimeInit, orchestration.RuntimeStart)
	require.Equal(t, expected, executed)
}

func TestPlaybookRunTwoDependenciesWithRestartWithPropagationInMiddleOnly(t *testing.T) {
	t.Skip("This test is not working as expected")
	ctx, done := common.NewContext()
	defer done()

	start := "web/gateway"
	accounts := "billing/accounts"
	org := "management/organization"

	wool.SetGlobalLogLevel(wool.DEBUG)

	exec := execOnInitThenPropagate()

	// We will propagate only for org after Init
	exec.onlyService = org

	data := setup(t, orchestration.RuntimeStart, exec)

	playbook, err := orchestration.NewPlaybook(ctx, data.world)
	require.NoError(t, err)

	playbook.WithPolicy(data.policy)

	playbook.WithSignallerFunc(func(_ *orchestration.Playbook, action orchestration.Action) *orchestration.Signal {
		if action.Type == orchestration.RuntimeStart && action.Service == start {
			return &orchestration.Signal{}
		}
		return nil
	})

	// Run
	stopped := make(chan error)
	go func() {
		// We don't stopAfter YET
		stopped <- playbook.Begin(ctx, orchestration.Action{Type: orchestration.RuntimeBegin, Service: start})
	}()

	// Block on signal
	for {
		select {
		case <-playbook.Signals():
			goto signalled
		}
	}
signalled:
	expected := createCombinedActionsWithRound(1, []string{org, accounts, start}, orchestration.RuntimeLoad, orchestration.RuntimeInit, orchestration.RuntimeStart)
	executed := playbook.Executed()
	require.Equal(t, expected, executed)

	// Now we will send a new action from the dependency of start
	playbook.WithStoppingAfter(func(ctx context.Context, action orchestration.Action) bool {
		return action.Type == orchestration.RuntimeStart && action.Service == accounts // We don't propagate all the way
	})

	// Re-init the "root": organization
	// Only org propagate so we shouldn't get any gateway
	err = playbook.Seed(ctx, orchestration.Action{Type: orchestration.RuntimeInit, Service: org})
	require.NoError(t, err)

	<-stopped
	// New ones
	executed = playbook.Executed()[len(executed):]

	expected = createCombinedActionsWithRound(2, []string{org, accounts}, orchestration.RuntimeInit, orchestration.RuntimeStart)
	require.Equal(t, expected, executed)
}

type onExecFailsOn struct {
	unique     string
	workingNow bool
	stage      orchestration.ActionType
}

func (o *onExecFailsOn) GetExecutor(ctx context.Context, action orchestration.Action) (orchestration.OutputProcessorFunc, error) {
	return func(ctx context.Context) (*orchestration.OutputProperty, error) {
		if action.Service == o.unique && action.Type == o.stage {
			if !o.workingNow {
				o.workingNow = true
				return orchestration.Pause(), nil
			} else {
				return orchestration.RequirePropagation(), nil
			}
		}
		return orchestration.OnInit(), nil
	}, nil
}

func execFailFirst(unique string, stage orchestration.ActionType) *onExecFailsOn {
	return &onExecFailsOn{
		unique: unique,
		stage:  stage,
	}
}

func TestErrorOnLoadNoDependencies(t *testing.T) {
	ctx := context.Background()
	wool.SetGlobalLogLevel(wool.DEBUG)
	start := "management/organization"

	data := setup(t, orchestration.RuntimeStart, execFailFirst(start, orchestration.RuntimeLoad))

	playbook, err := orchestration.NewPlaybook(ctx, data.world)
	require.NoError(t, err)

	playbook.WithPolicy(data.policy)

	playbook.WithSignallerFunc(func(_ *orchestration.Playbook, action orchestration.Action) *orchestration.Signal {
		if action.Failed {
			return &orchestration.Signal{}
		}
		return nil
	})

	// Run
	stopped := make(chan error)
	go func() {
		// We don't stopAfter YET
		stopped <- playbook.Begin(ctx, orchestration.Action{Type: orchestration.RuntimeBegin, Service: start})
	}()

	// Block on signal
	for {
		select {
		case <-playbook.Signals():
			goto signalled
		}
	}
signalled:
	expected := createCombinedActionsWithRound(1, []string{start}, orchestration.RuntimeLoad)
	executed := playbook.Executed()
	require.Equal(t, expected, executed)

	// Now we will send a new action from the dependency of start
	playbook.WithStoppingAfter(func(ctx context.Context, action orchestration.Action) bool {
		return action.Type == orchestration.RuntimeStart && action.Service == start
	})

	err = playbook.Seed(ctx, orchestration.Action{Type: orchestration.RuntimeLoad, Service: start})
	require.NoError(t, err)

	<-stopped
	// New ones
	executed = playbook.Executed()[len(executed):]
	// We don't propagate to start
	expected = createCombinedActionsWithRound(2, []string{start}, orchestration.RuntimeLoad, orchestration.RuntimeInit, orchestration.RuntimeStart)
	require.Equal(t, expected, executed)

}

func TestErrorOnLoadOneDependency(t *testing.T) {
	ctx := context.Background()
	wool.SetGlobalLogLevel(wool.DEBUG)

	start := "billing/accounts"
	org := "management/organization"

	data := setup(t, orchestration.RuntimeStart, execFailFirst(org, orchestration.RuntimeInit))

	playbook, err := orchestration.NewPlaybook(ctx, data.world)
	require.NoError(t, err)
	playbook.WithPolicy(data.policy)

	playbook.WithSignallerFunc(func(_ *orchestration.Playbook, action orchestration.Action) *orchestration.Signal {
		if action.Failed {
			return &orchestration.Signal{}
		}
		return nil
	})

	// Run
	stopped := make(chan error)
	go func() {
		// We don't stopAfter YET
		stopped <- playbook.Begin(ctx, orchestration.Action{Type: orchestration.RuntimeBegin, Service: start})
	}()

	// Block on signal
	for {
		select {
		case <-playbook.Signals():
			goto signalled
		}
	}
signalled:
	expected := createCombinedActionsWithRound(1, []string{org, start}, orchestration.RuntimeLoad)
	// Fail at org
	expected = append(expected, createActionsWithRound(1, org, orchestration.RuntimeInit)...)
	executed := playbook.Executed()
	require.Equal(t, expected, executed)

	// Now we will send a new action from the dependency of start
	playbook.WithStoppingAfter(func(ctx context.Context, action orchestration.Action) bool {
		return action.Type == orchestration.RuntimeStart && action.Service == start
	})

	err = playbook.Seed(ctx, orchestration.Action{Type: orchestration.RuntimeInit, Service: org})
	require.NoError(t, err)

	<-stopped
	// New ones
	executed = playbook.Executed()[len(executed):]
	expected = createCombinedActionsWithRound(2, []string{org, start}, orchestration.RuntimeInit, orchestration.RuntimeStart)
	require.Equal(t, expected, executed)

}
