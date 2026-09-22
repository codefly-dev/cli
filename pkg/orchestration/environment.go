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

// SelectedFixture resolves the fixture a workspace-bound flow runs with. An
// environment declares the fixture its runtime should use, so an ordinary
// `codefly run service` / `codefly test service` needs no --fixture at all.
// An explicit override always wins, and an environment that deliberately
// omits a fixture — because it loads a real provider — resolves to none.
func SelectedFixture(environment *environments.Environment, override string) string {
	if override != "" {
		return override
	}
	if environment == nil {
		return ""
	}
	return environment.Fixture
}
