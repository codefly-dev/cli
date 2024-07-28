package common

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	resources "github.com/codefly-dev/core/resources"
)

func LoadRequired(ctx context.Context, args []string) (*resources.Workspace, *resources.Module, *resources.Service) {
	workspace := RequireWorkspace(ctx)
	if len(args) == 0 {
		service := RequireService(ctx)
		module := RequireModule(ctx)
		service.WithModule(module.Name)
		return workspace, module, service
	}
	service, module, err := workspace.FindUniqueModuleServiceByName(ctx, args[0])
	if err != nil {
		cli.ExitOnError(err, "Cannot parse service argument")
	}
	return workspace, module, service
}

func Load(ctx context.Context, args []string) (*resources.Workspace, *resources.Module, *resources.Service) {
	workspace := RequireWorkspace(ctx)
	if len(args) == 0 {
		module := Module(ctx)
		service := Service(ctx)
		return workspace, module, service
	}
	service, module, err := workspace.FindUniqueModuleServiceByName(ctx, args[0])
	if err != nil {
		cli.ExitOnError(err, "Cannot parse service argument")
	}
	return workspace, module, service
}
