package common

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/configurations"
)

func Service(ctx context.Context) *configurations.Service {
	service, err := configurations.LoadServiceFromPath(ctx)
	cli.ExitOnError(err, "cannot load application from path")
	if service != nil {
		return service
	}
	app := Application(ctx)

	if app.ActiveService(ctx) == nil {
		cli.Error("couldn't find a service to run from active application")
		cli.Exit()

	}

	service, err = app.LoadActiveService(ctx)
	cli.ExitOnError(err, "cannot get current service")
	return service
}
