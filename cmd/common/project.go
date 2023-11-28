package common

import (
	"github.com/codefly-dev/core/configurations"
)

func ProjectConfiguration(current bool) *configurations.Project {
	project, _ := configurations.ProjectConfiguration(current)
	return project
}
