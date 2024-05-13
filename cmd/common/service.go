package common

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	resources "github.com/codefly-dev/core/resources"
)

func LoadRequired(ctx context.Context, args []string) (*resources.Workspace, *resources.Service) {
	workspace := RequireWorkspace(ctx)
	if len(args) == 0 {
		service := RequireService(ctx)
		return workspace, service
	}
	service, err := workspace.FindUniqueServiceByName(ctx, args[0])
	if err != nil {
		cli.ExitOnError(err, "Cannot parse service argument")
	}
	return workspace, service
}

func Load(ctx context.Context, args []string) (*resources.Workspace, *resources.Service) {
	workspace := RequireWorkspace(ctx)
	if len(args) == 0 {
		service := Service(ctx)
		return workspace, service
	}
	service, err := workspace.FindUniqueServiceByName(ctx, args[0])
	if err != nil {
		cli.ExitOnError(err, "Cannot parse service argument")
	}
	return workspace, service
}
