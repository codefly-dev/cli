package orchestration

import (
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// Resolution and validation of run profiles live in core
// (resources.Workspace.ResolveRunProfile). The CLI only owns the policy that a
// resolved profile applies to run flows and nothing else, so that is all this
// test covers.
func TestRunProfileCannotApplyToDeploymentFlow(t *testing.T) {
	flow := &Flow{world: &World{Mode: DeployMode}}
	err := flow.WithRunProfile(resources.RunProfile{
		ExcludeDependencies:            []string{"app/managed"},
		ExcludeWorkspaceConfigurations: []string{"managed-auth"},
	})
	require.ErrorContains(t, err, "only be applied to run flows")
	require.Empty(t, flow.excludedDependencyServices)
	require.Empty(t, flow.world.excludedWorkspaceConfigurations)
}
