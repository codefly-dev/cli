package common

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/configurations"
)

type ActiveContext struct {
	Project     *configurations.Project
	Application *configurations.Application
	Service     *configurations.Service
}

func LoadActiveContext(ctx context.Context) (*ActiveContext, error) {
	workspace, err := configurations.LoadWorkspace(ctx, configurations.LocalWorkspace)
	if err != nil {
		return nil, err
	}
	// Override path
	project, err := configurations.LoadProjectFromPath(ctx)
	if err != nil {
		return nil, err
	}
	if project == nil {
		project, err = workspace.LoadActiveProject(ctx)
		if err != nil {
			return nil, err
		}
	}
	active := &ActiveContext{
		Project: project,
	}
	application, err := configurations.LoadApplicationFromPath(ctx)
	if err != nil {
		return nil, err
	}
	if application == nil {
		application, err = workspace.LoadActiveApplication(ctx, project.Name)
		if err != nil {
			return active, nil
		}
	}
	active.Application = application

	service, err := configurations.LoadServiceFromPath(ctx)
	if err != nil {
		return nil, err
	}
	if service == nil {
		service, err = workspace.LoadActiveService(ctx, project.Name, application.Name)
		if err != nil {
			return active, nil
		}
	}
	active.Service = service
	return active, nil
}

func Service(ctx context.Context) *configurations.Service {
	active, err := LoadActiveContext(ctx)
	cli.ExitOnError(err, "cannot load active context")
	cli.ExitIf(active.Service == nil, "no active service")
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
