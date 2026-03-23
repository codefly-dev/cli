package common

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/cli"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/tui"
)

var _active *ActiveContext

type ActiveContext struct {
	Workspace *resources.Workspace
	Module    *resources.Module
	Service   *resources.Service
}

func LoadActiveContext(ctx context.Context) (*ActiveContext, error) {
	if _active != nil {
		return _active, nil
	}
	active := &ActiveContext{}

	workspace, err := resources.FindWorkspaceUp(ctx)
	if err != nil {
		return nil, err
	}

	if workspace == nil {
		return nil, fmt.Errorf("no workspace found")
	}
	active.Workspace = workspace

	if workspace.Layout == resources.LayoutKindFlat {
		module, err := workspace.LoadModuleFromName(ctx, workspace.Name)
		if err != nil {
			return nil, err
		}
		active.Module = module
		service, err := resources.LoadServiceFromCurrentPath(ctx)
		if err != nil {
			return nil, err
		}
		active.Service = service
	} else {
		module, service, err := resources.LoadModuleAndServiceFromCurrentPath(ctx)
		if err != nil {
			return nil, err
		}

		active.Module = module
		active.Service = service
	}

	if active.Service == nil {
		active.Service, active.Module, err = autoResolveService(ctx, workspace)
		if err != nil {
			return nil, err
		}
	}

	_active = active
	return active, nil
}

// autoResolveService picks a service when the user isn't inside a service
// folder. If exactly one service exists, it's selected automatically.
// If multiple exist, an interactive prompt lets the user choose.
func autoResolveService(ctx context.Context, workspace *resources.Workspace) (*resources.Service, *resources.Module, error) {
	services, err := workspace.LoadServices(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(services) == 0 {
		return nil, nil, nil
	}

	var picked *resources.Service
	if len(services) == 1 {
		picked = services[0]
	} else {
		entries := make([]*tui.Entry, len(services))
		for i, svc := range services {
			entries[i] = &tui.Entry{Identifier: svc.Name}
		}
		result, selectErr := tui.RunSelect("Multiple services found — pick one:", entries)
		if selectErr != nil {
			return nil, nil, selectErr
		}
		if result.Stopped {
			cli.Exit()
		}
		for _, svc := range services {
			if svc.Name == result.Entry.Identifier {
				picked = svc
				break
			}
		}
	}
	if picked == nil {
		return nil, nil, nil
	}

	for _, modRef := range workspace.Modules {
		mod, loadErr := workspace.LoadModuleFromReference(ctx, modRef)
		if loadErr != nil {
			continue
		}
		for _, svcRef := range mod.ServiceReferences {
			if svcRef.Name == picked.Name {
				picked.WithModule(mod.Name)
				return picked, mod, nil
			}
		}
	}
	return picked, nil, nil
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
