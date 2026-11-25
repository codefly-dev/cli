package orchestration

import (
	"context"
	"errors"
	"testing"

	agentv1 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v1"
	"github.com/codefly-dev/core/tui"
	"github.com/stretchr/testify/require"
)

type testStartObserver struct {
	states []tui.ServiceState
	calls  int
	output *OutputProperty
	err    error
}

func (observer *testStartObserver) GetExecutor(context.Context, Action) (OutputProcessorFunc, error) {
	return func(context.Context) (*OutputProperty, error) {
		observer.calls++
		return observer.output, observer.err
	}, nil
}
func (observer *testStartObserver) ServicePort(string) int { return 0 }
func (observer *testStartObserver) emitState(_ string, state tui.ServiceState, _ int) {
	observer.states = append(observer.states, state)
}

func TestRuntimeTestPolicyObservesOnlyCompletedProcessStarts(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		mode    agentv1.TestDependencyMode
		service string
		output  *OutputProperty
		err     error
		calls   int
		running bool
	}{
		{"dependency", agentv1.TestDependencyMode_TEST_DEPENDENCY_MODE_START_DEPENDENCIES, "host/api", OnInit(), nil, 1, true},
		{"target barrier", agentv1.TestDependencyMode_TEST_DEPENDENCY_MODE_START_DEPENDENCIES, "app/api", OnInit(), nil, 0, false},
		{"full stack target", agentv1.TestDependencyMode_TEST_DEPENDENCY_MODE_START_STACK, "app/api", OnInit(), nil, 1, true},
		{"failed", agentv1.TestDependencyMode_TEST_DEPENDENCY_MODE_START_STACK, "app/api", nil, errors.New("refused"), 1, false},
		{"cancelled", agentv1.TestDependencyMode_TEST_DEPENDENCY_MODE_START_STACK, "app/api", nil, nil, 1, false},
		{"waiting", agentv1.TestDependencyMode_TEST_DEPENDENCY_MODE_START_STACK, "app/api", Pause(), nil, 1, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			observer := &testStartObserver{output: scenario.output, err: scenario.err}
			policy, err := NewRuntimeTestPolicy(context.Background(), nil, observer, "app/api", scenario.mode)
			require.NoError(t, err)
			_, err = policy.Execute(context.Background(), Action{Type: RuntimeStart, Service: scenario.service})
			if scenario.err != nil {
				require.ErrorIs(t, err, scenario.err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, scenario.calls, observer.calls)
			if scenario.running {
				require.Equal(t, []tui.ServiceState{tui.StateRunning}, observer.states)
			} else {
				require.Empty(t, observer.states)
			}
		})
	}
}
