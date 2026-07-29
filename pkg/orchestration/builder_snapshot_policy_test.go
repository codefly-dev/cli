package orchestration_test

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/stretchr/testify/require"
)

func TestSnapshotPolicyBuildsBeforeDeploy(t *testing.T) {
	ctx := context.Background()
	data := setup(t, orchestration.BuilderSync, execOnInit())
	policy, err := orchestration.NewSnapshotPolicy(ctx, data.world.Dependencies, execOnInit())
	require.NoError(t, err)

	start := "management/organization"
	require.NoError(t, policy.Restrict(ctx, start))

	action := orchestration.Action{Type: orchestration.BuilderBegin, Service: start}
	for _, want := range []orchestration.ActionType{
		orchestration.BuilderLoad,
		orchestration.BuilderInit,
		orchestration.BuilderBuild,
		orchestration.BuilderDeploy,
	} {
		actions, err := policy.Execute(ctx, action)
		require.NoError(t, err)
		require.Equal(t, createActions(start, want), actions)
		action = actions[0]
	}

	actions, err := policy.Execute(ctx, action)
	require.NoError(t, err)
	require.Empty(t, actions)
}
