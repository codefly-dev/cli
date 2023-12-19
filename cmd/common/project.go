package common

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/configurations"
)

func Project(ctx context.Context) *configurations.Project {
	workspace, err := configurations.LoadWorkspace(ctx)
	cli.ExitOnError(err, "cannot get current workspace")
	project, err := configurations.LoadProjectFromPath(ctx)
	cli.ExitOnError(err, "cannot load project")
	if project != nil {
		return project
	}
	project, err = workspace.LoadActiveProject(ctx)
	cli.ExitOnError(err, "cannot get current project")
	return project
}
