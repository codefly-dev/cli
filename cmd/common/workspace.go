package common

import (
	"context"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

func Workspace(ctx context.Context) *configurations.Workspace {
	workspace, err := configurations.LoadWorkspace(ctx)
	shared.UnexpectedExitOnError(err, "cannot get current workspace")
	return workspace
}
