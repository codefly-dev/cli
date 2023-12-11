package application

import (
	"context"

	"github.com/codefly-dev/cli/pkg/services"
	"github.com/codefly-dev/core/agents/endpoints"
	factoryv1 "github.com/codefly-dev/core/proto/v1/go/services/factory"

	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
)

func (app *Application) Build(ctx context.Context) error {
	logger := shared.GetLogger(ctx).With("applications.Build<%s>", app.Configuration.Name)
	for _, service := range app.Plan.Services {
		err := app.BuildService(ctx, service)
		if err != nil {
			return logger.Wrapf(err, "cannot build service <%s>", service.Configuration.Name)
		}
	}
	return nil
}

func (app *Application) BuildService(ctx context.Context, instance *services.Instance) error {
	logger := shared.GetLogger(ctx).With("applications.BuildService<%s>", instance.Configuration.Name)
	if instance.Factory == nil {
		return logger.Errorf("runtime for instance <%s> is not initialized, run first app.Init()", instance.Configuration.Name)
	}

	ShowEndpointManagerState(ctx)
	group, err := GetEndpointDependencyGroup(ctx, instance.Configuration)
	if err != nil {
		return logger.Wrapf(err, "cannot get application group endpoints")
	}

	logger.Debugf("dependency group: %v", endpoints.CondensedOutput(group))

	golor.Println(`#(bold,cyan)[Building {{.Name}}]`, instance.Configuration)
	_, err = instance.Build(ctx, &factoryv1.BuildRequest{
		DependencyEndpointGroup: group,
	})
	if err != nil {
		return logger.Wrapf(err, "cannot build runtime")
	}
	golor.Println(`#(bold,cyan)[Build {{.Name}}]: #(green)[OK]`, instance.Configuration)
	return nil
}
