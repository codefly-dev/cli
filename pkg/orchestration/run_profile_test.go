package orchestration

import (
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// Resolution and validation of run profiles live in core
// (resources.Workspace.ResolveRunProfile). The CLI only owns the policy that a
// resolved profile applies to local run and test flows, so that is all these
// tests cover. WithRunProfile is the single sanctioned way to trim run
// composition, so both its accept and reject paths are pinned here.

// A run flow adopts the resolved profile's canonical dependency and workspace
// configuration exclusions, which the graph builder and the workspace
// configuration projection then consume.
func TestRunProfileAppliesExclusionsToLocalRuntimeFlows(t *testing.T) {
	for _, mode := range []Mode{RunMode, TestMode} {
		t.Run(string(mode), func(t *testing.T) {
			flow := &Flow{world: &World{Mode: mode}}
			require.NoError(t, flow.WithRunProfile(resources.RunProfile{}))
			err := flow.WithRunProfile(resources.RunProfile{
				ExcludeDependencies:            []string{"app/managed"},
				ExcludeWorkspaceConfigurations: []string{"managed-auth"},
			})
			require.NoError(t, err)
			require.Equal(t, []string{"app/managed"}, flow.excludedDependencyServices)
			require.Equal(t, map[string]bool{"managed-auth": true}, flow.world.excludedWorkspaceConfigurations)
		})
	}
}

// Profiles only trim local runtime composition, so applying one to a deployment
// is rejected and leaves the flow untouched.
func TestRunProfileCannotApplyToNonRuntimeFlows(t *testing.T) {
	for _, mode := range []Mode{DeployMode, BuildMode, SyncMode, SnapshotMode, LintMode, CompileMode} {
		t.Run(string(mode), func(t *testing.T) {
			flow := &Flow{world: &World{Mode: mode}}
			err := flow.WithRunProfile(resources.RunProfile{
				ExcludeDependencies:            []string{"app/managed"},
				ExcludeWorkspaceConfigurations: []string{"managed-auth"},
			})
			require.ErrorContains(t, err, "only be applied to run or test flows")
			require.Empty(t, flow.excludedDependencyServices)
			require.Empty(t, flow.world.excludedWorkspaceConfigurations)
		})
	}
}
