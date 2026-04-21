package common

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	resources "github.com/codefly-dev/core/resources"
)

func LoadRequired(ctx context.Context, args []string) (*resources.Workspace, *resources.Module, *resources.Service) {
	if len(args) > 0 {
		// Service name provided — just load the workspace and look it up directly.
		// Don't trigger auto-resolve/picker.
		workspace, err := resources.FindWorkspaceUp(ctx)
		cli.ExitOnError(err, "Cannot find workspace")
		if workspace == nil {
			cli.ExitWithMessage("No workspace found")
		}
		service, module, err := workspace.FindUniqueModuleServiceByName(ctx, args[0])
		cli.ExitOnError(err, "Cannot find service %s", args[0])
		return workspace, module, service
	}
	// No args — use active context (may prompt for selection).
	workspace := RequireWorkspace(ctx)
	service := RequireService(ctx)
	module := RequireModule(ctx)
	service.WithModule(module.Name)
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
