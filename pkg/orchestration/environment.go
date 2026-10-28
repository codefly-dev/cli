package orchestration

import (
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
)

// LocalEnvironmentName is the environment every workspace-bound lifecycle
// command runs against unless the user selects another one with --env.
const LocalEnvironmentName = "local"

// SelectEnvironment is the canonical environment-selection path for
// workspace-bound flows. It honors the workspace's declared environment when
// present (CLI selection keeps the synthetic "local" default for
// workspaces that never declared one) and fails — before any agent is
// spawned — when a non-local environment is requested but not declared.
//
// The returned Environment is a deep copy: invocation-scoped overrides such
// as --naming-scope must not leak into the declaration shared by concurrent
// flows over the same Workspace.
func SelectEnvironment(workspace *resources.Workspace, name string) (*environments.Environment, error) {
	return environments.Select(workspace, name)
}

// SelectedFixture resolves the fixture a workspace-bound flow runs with,
// where the environment declares it (environments.SelectedFixture, which the
// composition package shares).
func SelectedFixture(environment *environments.Environment, override string) string {
	return environments.SelectedFixture(environment, override)
}
