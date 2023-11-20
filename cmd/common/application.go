package common

import (
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

func ApplicationConfiguration(current bool) *configurations.Application {
	app, err := configurations.ApplicationConfiguration(current)
	shared.UnexpectedExitOnError(err, "cannot load application configuration")
	return app
}
