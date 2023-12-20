package common

import (
	"context"

	"github.com/codefly-dev/core/configurations"
)

type ActiveContext struct {
	Project     string
	Application string
	Service     string
}

var active *ActiveContext

func LoadActiveContext(ctx context.Context) (*ActiveContext, error) {
	if active != nil {
		return active, nil
	}
	workspace, err := configurations.LoadWorkspace(ctx)
	if err != nil {
		return nil, err
	}
	project, err := workspace.LoadActiveProject(ctx)
	if err != nil {
		return nil, err
	}
	active := &ActiveContext{
		Project: project.Name,
	}
	application, err := project.LoadActiveApplication(ctx)
	if err != nil {
		return active, nil
	}
	active.Application = application.Name
	service, err := application.LoadActiveService(ctx)
	if err != nil {
		return active, nil
	}
	active.Service = service.Name
	return active, nil
}
