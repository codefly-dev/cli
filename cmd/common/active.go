package common

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/wool"
)

type ActiveContext struct {
	Workspace   *configurations.Workspace
	Project     *configurations.Project
	Application *configurations.Application
	Service     *configurations.Service
}

var _active *ActiveContext

func LoadActiveContext(ctx context.Context) (*ActiveContext, error) {
	w := wool.Get(ctx).In("loadActiveContext")
	if _active != nil {
		return _active, nil
	}
	active := &ActiveContext{}
	workspace, err := configurations.LoadWorkspace(ctx, configurations.LocalWorkspace)
	if err != nil {
		w.Warn("running without workspace: run `codefly init` for context magic")
	}

	active.Workspace = workspace
	// Override path
	project, err := configurations.LoadProjectFromPath(ctx)
	if err != nil {
		return nil, err
	}
	pathProjectOverride := project != nil

	if project == nil {
		if workspace != nil {
			project, err = workspace.LoadActiveProject(ctx)
			if err != nil {
				return nil, err
			}
		}
	}
	active.Project = project
	application, err := configurations.LoadApplicationFromPath(ctx)
	if err != nil {
		return nil, err
	}
	pathApplicationOverride := application != nil
	if application == nil {
		if !pathProjectOverride && workspace != nil {
			if project != nil {
				application, err = workspace.LoadActiveApplication(ctx, project.Name)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	active.Application = application

	service, err := configurations.LoadServiceFromPath(ctx)
	if err != nil {
		return nil, err
	}
	if service == nil {
		if !pathApplicationOverride && workspace != nil {
			if project != nil && application != nil {
				service, err = workspace.LoadActiveService(ctx, project.Name, application.Name)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	active.Service = service
	_active = active
	return active, nil
}

func Service(ctx context.Context) *configurations.Service {
	active, err := LoadActiveContext(ctx)
	cli.ExitOnError(err, "cannot load active context")
	return active.Service
}

func Application(ctx context.Context) *configurations.Application {
	active, err := LoadActiveContext(ctx)
	cli.ExitOnError(err, "cannot load active context")
	cli.ExitIf(active.Application == nil, "no active application")
	return active.Application
}

func Project(ctx context.Context) *configurations.Project {
	active, err := LoadActiveContext(ctx)
	cli.ExitOnError(err, "cannot load active context")
	cli.ExitIf(active.Project == nil, "no active project")
	return active.Project
}

func Workspace(ctx context.Context) *configurations.Workspace {
	active, err := LoadActiveContext(ctx)
	cli.ExitOnError(err, "cannot load active context")
	return active.Workspace
}
