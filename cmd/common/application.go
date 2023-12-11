package common

import (
	"context"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

func Application(ctx context.Context) *configurations.Application {
	app, err := configurations.LoadApplicationFromPath(ctx)
	shared.UnexpectedExitOnError(err, "cannot load application from path")
	if app != nil {
		return app
	}
	w, err := configurations.LoadWorkspace(ctx)
	shared.UnexpectedExitOnError(err, "cannot get current workspace")

	project, err := w.LoadActiveProject(ctx)
	shared.UnexpectedExitOnError(err, "cannot get current project")

	if project.ActiveApplication() == nil {
		shared.Exit("couldn't find an application to run from active project")

	}
	app, err = project.LoadActiveApplication(ctx)
	shared.UnexpectedExitOnError(err, "cannot get current application")
	return app
}
