package common

import (
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

func ProjectConfiguration(current bool) *configurations.Project {
	project, err := configurations.ProjectConfiguration(current)

	shared.UnexpectedExitOnError(err, "cannot load project configuration")
	return project
}
