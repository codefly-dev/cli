package common

import (
	"context"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

func Service(ctx context.Context) *configurations.Service {
	service, err := configurations.LoadServiceFromPath(ctx)
	shared.UnexpectedExitOnError(err, "cannot load application from path")
	if service != nil {
		return service
	}
	app := Application(ctx)

	if app.ActiveService(ctx) == nil {
		shared.Exit("couldn't find a service to run from active application")

	}

	service, err = app.LoadActiveService(ctx)
	shared.UnexpectedExitOnError(err, "cannot get current service")
	return service
}
