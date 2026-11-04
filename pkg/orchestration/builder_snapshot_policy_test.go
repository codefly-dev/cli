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
		orchestration.BuilderPlan,
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

// The build phase is the one phase a snapshot emits whole, so the playbook can
// run its services at the same time; every other phase still arrives one
// service at a time.
func TestSnapshotPolicyEmitsTheBuildPhaseAsOneGroup(t *testing.T) {
	ctx := context.Background()
	data := setup(t, orchestration.BuilderSync, execOnInit())
	policy, err := orchestration.NewSnapshotPolicy(ctx, data.world.Dependencies, execOnInit())
	require.NoError(t, err)

	// web/frontend -> web/gateway -> billing/accounts -> management/organization
	start := "web/frontend"
	require.NoError(t, policy.Restrict(ctx, start))

	group := []orchestration.Action{{Type: orchestration.BuilderBegin, Service: start}}
	seen := map[orchestration.ActionType][][]orchestration.Action{}
	for range 64 {
		var next []orchestration.Action
		for _, action := range group {
			emitted, execErr := policy.Execute(ctx, action)
			require.NoError(t, execErr)
			for _, candidate := range emitted {
				if !containsAction(next, candidate) {
					next = append(next, candidate)
				}
			}
		}
		if len(next) == 0 {
			break
		}
		seen[next[0].Type] = append(seen[next[0].Type], next)
		group = next
	}

	// Every phase but the build phase still arrives one service at a time.
	for _, phase := range []orchestration.ActionType{
		orchestration.BuilderLoad, orchestration.BuilderInit,
		orchestration.BuilderPlan, orchestration.BuilderDeploy,
	} {
		groups := seen[phase]
		require.Len(t, groups, 4, "%s should arrive one service at a time", phase)
		for _, one := range groups {
			require.Len(t, one, 1, "%s should arrive one service at a time", phase)
		}
	}

	// The build phase arrives once, holding every service of the closure.
	builds := seen[orchestration.BuilderBuild]
	require.Len(t, builds, 1, "the build phase should be emitted once, whole")
	require.Len(t, builds[0], 4, "the build phase should carry every service of the closure")

	// Every build of that group names the same successor, so the phase after it
	// is entered exactly once whichever build finished last. Without this, the
	// deploy phase would either start four times or — if each build named its
	// sibling — depend on the last build being the one to emit it.
	successors := map[string]bool{}
	for _, build := range builds[0] {
		emitted, execErr := policy.Execute(ctx, build)
		require.NoError(t, execErr)
		require.Len(t, emitted, 1)
		successors[string(emitted[0].Type)+":"+emitted[0].Service] = true
	}
	require.Len(t, successors, 1, "the builds of one phase named different successors")
}

func containsAction(actions []orchestration.Action, candidate orchestration.Action) bool {
	for _, action := range actions {
		if action.Type == candidate.Type && action.Service == candidate.Service {
			return true
		}
	}
	return false
}
