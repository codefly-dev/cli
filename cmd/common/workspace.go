package common

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/configurations"
)

func Workspace(ctx context.Context) *configurations.Workspace {
	workspace, err := configurations.LoadWorkspace(ctx)
	cli.ExitOnError(err, "cannot get current workspace")
	return workspace
}
