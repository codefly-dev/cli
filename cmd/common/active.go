package common

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	resources "github.com/codefly-dev/core/resources"
)

type ActiveContext struct {
	Workspace *resources.Workspace
	Module    *resources.Module
	Service   *resources.Service
}

var _active *ActiveContext

func LoadActiveContext(ctx context.Context) (*ActiveContext, error) {
	if _active != nil {
		return _active, nil
	}
	active := &ActiveContext{}

	// Override path
	workspace, err := resources.FindWorkspaceUp(ctx)
	if err != nil {
		return nil, err
	}

	active.Workspace = workspace

	module, service, err := resources.LoadModuleAndServiceFromCurrentPath(ctx)
	if err != nil {
		return nil, err
	}

	active.Module = module
	active.Service = service

	_active = active
	return active, nil
}

func Service(ctx context.Context) *resources.Service {
	active, err := LoadActiveContext(ctx)
	cli.ExitOnError(err, "cannot load active context")
	return active.Service
}

func RequireService(ctx context.Context) *resources.Service {
	service := Service(ctx)
	if service == nil {
		cli.Error("No service found: run inside a service folder or use workspace")
		cli.Exit()
	}
	return service
}

func Module(ctx context.Context) *resources.Module {
	active, err := LoadActiveContext(ctx)
	cli.ExitOnError(err, "cannot load active context")
	return active.Module
}

func RequireModule(ctx context.Context) *resources.Module {
	module := Module(ctx)
	if module == nil {
		cli.Error("No module found: run inside an module folder or use workspace")
		cli.Exit()
	}
	return module
}

func Workspace(ctx context.Context) *resources.Workspace {
	active, err := LoadActiveContext(ctx)
	cli.ExitOnError(err, "cannot load active context")
	return active.Workspace
}

func RequireWorkspace(ctx context.Context) *resources.Workspace {
	workspace := Workspace(ctx)
	if workspace == nil {
		cli.Error("No workspace found: run inside a workspace folder or use workspace")
		cli.Exit()
	}
	return workspace
}
