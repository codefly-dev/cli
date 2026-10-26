package orchestration

import (
	"context"
	"errors"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestCompositionPreflightRunsBeforeFlowAndRechecksRefresh(t *testing.T) {
	ctx := t.Context()
	workspace := multiRootWorkspace(t)
	module, err := workspace.LoadModuleFromName(ctx, "wiki")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "backend")
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	refusal := errors.New("invalid delivered permission")
	calls := 0
	reject := true
	check := func(_ context.Context, w *resources.Workspace, _ *environments.Environment) error {
		calls++
		require.ElementsMatch(t, []string{"wiki", "saas", "documents"}, moduleNames(w))
		if reject {
			return refusal
		}
		return nil
	}
	flow, err := NewFlow(ctx, workspace, module, service, env, RunMode, WithRunModuleClosure("wiki"), WithCompositionCheck(check))
	require.ErrorIs(t, err, refusal)
	require.Nil(t, flow)
	require.Equal(t, 1, calls)
	reject = false
	flow, err = NewFlow(ctx, workspace, module, service, env, RunMode, WithRunModuleClosure("wiki"), WithCompositionCheck(check))
	require.NoError(t, err)
	reject = true
	require.ErrorIs(t, flow.CheckComposition(ctx, workspace), refusal)
	require.Equal(t, 3, calls)
}
