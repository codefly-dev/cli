package common

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/configurations"
)

func Application(ctx context.Context) *configurations.Application {
	app, err := configurations.LoadApplicationFromPath(ctx)
	cli.ExitOnError(err, "cannot load application from path")
	if app != nil {
		return app
	}
	w, err := configurations.LoadWorkspace(ctx)
	cli.ExitOnError(err, "cannot get current workspace")

	project, err := w.LoadActiveProject(ctx)
	cli.ExitOnError(err, "cannot get current project")

	if project.ActiveApplication() == nil {
		cli.Error("couldn't find an application to run from active project")
		cli.Exit()

	}
	app, err = project.LoadActiveApplication(ctx)
	cli.ExitOnError(err, "cannot get current application")
	return app
}
