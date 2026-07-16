package orchestration_test

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/architecture"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

type testPolicyRecorder struct {
	actions []orchestration.Action
}

func (recorder *testPolicyRecorder) GetExecutor(_ context.Context, action orchestration.Action) (orchestration.OutputProcessorFunc, error) {
	recorder.actions = append(recorder.actions, action)
	return func(context.Context) (*orchestration.OutputProperty, error) {
		return orchestration.OnInit(), nil
	}, nil
}

func runTestPolicy(t *testing.T, mode agentv0.TestDependencyMode) ([]orchestration.Action, []orchestration.Action) {
	t.Helper()
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, "testdata/module-layout")
	require.NoError(t, err)
	dependencies, err := architecture.NewServiceDependencies(ctx, workspace)
	require.NoError(t, err)

	origin := "web/gateway"
	recorder := &testPolicyRecorder{}
	policy, err := orchestration.NewRuntimeTestPolicy(ctx, dependencies, recorder, origin, mode)
	require.NoError(t, err)
	playbook, err := orchestration.NewPlaybook(ctx, &orchestration.World{Dependencies: dependencies})
	require.NoError(t, err)
	playbook.WithPolicy(policy)
	playbook.WithStoppingAfter(func(_ context.Context, action orchestration.Action) bool {
		return action.Type == orchestration.RuntimeTest && action.Service == origin
	})
	require.NoError(t, playbook.Begin(ctx, orchestration.Action{Type: orchestration.RuntimeBegin, Service: origin}))
	return playbook.Executed(), recorder.actions
}

func TestRuntimeTestPolicyNoneInitializesOnlyTarget(t *testing.T) {
	executed, _ := runTestPolicy(t, agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE)
	want := createActionsWithRound(1, "web/gateway", orchestration.RuntimeLoad, orchestration.RuntimeInit, orchestration.RuntimeTest)
	require.Equal(t, want, executed)
}

func TestRuntimeTestPolicyStartsDependenciesWithoutTarget(t *testing.T) {
	executed, dispatched := runTestPolicy(t, agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_START_DEPENDENCIES)
	wantExecuted := createCombinedActionsWithRound(
		1,
		[]string{"management/organization", "billing/accounts", "web/gateway"},
		orchestration.RuntimeLoad,
		orchestration.RuntimeInit,
		orchestration.RuntimeStart,
	)
	wantExecuted = append(wantExecuted, orchestration.Action{Type: orchestration.RuntimeTest, Service: "web/gateway", Round: 1})
	require.Equal(t, wantExecuted, executed)
	for _, action := range dispatched {
		require.False(t, action.Type == orchestration.RuntimeStart && action.Service == "web/gateway", "target start reached executor")
	}
	assertContainsAction(t, dispatched, orchestration.RuntimeStart, "management/organization")
	assertContainsAction(t, dispatched, orchestration.RuntimeStart, "billing/accounts")
}

func TestRuntimeTestPolicyStartsCompleteStack(t *testing.T) {
	_, dispatched := runTestPolicy(t, agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_START_STACK)
	assertContainsAction(t, dispatched, orchestration.RuntimeStart, "management/organization")
	assertContainsAction(t, dispatched, orchestration.RuntimeStart, "billing/accounts")
	assertContainsAction(t, dispatched, orchestration.RuntimeStart, "web/gateway")
	assertContainsAction(t, dispatched, orchestration.RuntimeTest, "web/gateway")
}

func assertContainsAction(t *testing.T, actions []orchestration.Action, actionType orchestration.ActionType, service string) {
	t.Helper()
	for _, action := range actions {
		if action.Type == actionType && action.Service == service {
			return
		}
	}
	t.Fatalf("actions %v do not contain %s for %s", actions, actionType, service)
}
