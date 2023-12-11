package common

import (
	"context"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

func Project(ctx context.Context) *configurations.Project {
	workspace, err := configurations.LoadWorkspace(ctx)
	shared.UnexpectedExitOnError(err, "cannot get current workspace")
	project, err := configurations.LoadProjectFromPath(ctx)
	shared.UnexpectedExitOnError(err, "cannot get current project")
	if project != nil {
		return project
	}
	project, err = workspace.LoadActiveProject(ctx)
	shared.UnexpectedExitOnError(err, "cannot get current project")
	return project
}
