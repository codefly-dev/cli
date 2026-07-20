package orchestration_test

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/stretchr/testify/require"
)

func TestSyncPolicyNoDependencies(t *testing.T) {
	ctx := context.Background()
	data := setup(t, orchestration.BuilderSync, execOnInit())

	start := "management/organization"

	err := data.policy.Restrict(ctx, start)
	require.NoError(t, err)

	actions, err := data.policy.Execute(ctx, orchestration.Action{Type: orchestration.BuilderBegin, Service: start})
	require.NoError(t, err)
	require.Equal(t, createActions(start, orchestration.BuilderLoad), actions, "Expected no action to be triggered")

	actions, err = data.policy.Execute(ctx, orchestration.Action{Type: orchestration.BuilderLoad, Service: start})
	require.NoError(t, err)
	require.Equal(t, createActions(start, orchestration.BuilderInit), actions, "Expected no action to be triggered")

	actions, err = data.policy.Execute(ctx, orchestration.Action{Type: orchestration.BuilderInit, Service: start})
	require.NoError(t, err)
	require.Equal(t, createActions(start, orchestration.BuilderSync), actions, "Expected no action to be triggered")

}

func TestSyncPolicyOneDependency(t *testing.T) {
	ctx := context.Background()
	data := setup(t, orchestration.BuilderSync, execOnInit())

	start := "billing/accounts"
	org := "management/organization"

	err := data.policy.Restrict(ctx, start)
	require.NoError(t, err)

	actions, err := data.policy.Execute(ctx, orchestration.Action{Type: orchestration.BuilderBegin, Service: start})
	require.NoError(t, err)
	require.Equal(t, createCombinedActions([]string{org, start}, orchestration.BuilderLoad), actions, "Expected no action to be triggered")

	actions, err = data.policy.Execute(ctx, orchestration.Action{Type: orchestration.BuilderInit, Service: org})
	require.NoError(t, err)
	require.Equal(t, createActions(org, orchestration.BuilderSync), actions)

	actions, err = data.policy.Execute(ctx, orchestration.Action{Type: orchestration.BuilderInit, Service: start})
	require.NoError(t, err)
	require.Equal(t, createCombinedActions([]string{org, start}, orchestration.BuilderSync), actions)
}
