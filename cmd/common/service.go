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

func LoadWithServicePathOverride(ctx context.Context, servicePath string) (*resources.Workspace, *resources.Module, *resources.Service) {
	workspace := RequireWorkspace(ctx)
	service, err := resources.LoadServiceFromDir(ctx, servicePath)
	if err != nil {
		cli.ExitOnError(err, "Cannot load service")
	}
	if workspace.Layout == resources.LayoutKindFlat {
		module, err := workspace.LoadModuleFromName(ctx, workspace.Name)
		if err != nil {
			cli.ExitOnError(err, "Cannot load module")
		}
		service.WithModule(module.Name)
		return workspace, module, service
	}
	cli.ExitWithMessage("Cannot load service in non-flat layout for now")
	return nil, nil, nil
}
