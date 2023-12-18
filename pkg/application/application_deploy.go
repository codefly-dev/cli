package application

import (
	"context"

	"github.com/codefly-dev/cli/pkg/services"
	"github.com/codefly-dev/core/agents/endpoints"
	"github.com/codefly-dev/core/configurations"
	basev1 "github.com/codefly-dev/core/generated/go/base/v1"
	factoryv1 "github.com/codefly-dev/core/generated/go/services/factory/v1"
	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/golor"
)

func (app *Application) Deploy(ctx context.Context, env *configurations.Environment) error {
	w := wool.Get(ctx).In("applications.Deploy<%s>", app.Configuration.Name)
	for _, service := range app.Plan.Services {
		err := app.DeployService(ctx, service, env)
		if err != nil {
			return logger.Wrapf(err, "cannot build service <%s>", service.Configuration.Name)
		}
	}
	return nil
}

func (app *Application) DeployService(ctx context.Context, instance *services.Instance, env *configurations.Environment) error {
	w := wool.Get(ctx).In("applications.DeployService<%s>", instance.Configuration.Name)
	if instance.Runtime == nil {
		return logger.Errorf("runtime for instance <%s> is not initialized, run first app.Init()", instance.Configuration.Name)
	}
	logger.TODO("Type of build will depend on deployment, right now assume we dockerize")
	// What kind of build will be picked from the deployment

	group, err := GetEndpointDependencyGroup(ctx, instance.Configuration)
	if err != nil {
		return logger.Wrapf(err, "cannot get application group endpoints")
	}

	logger.Debugf("dependency group: %v", endpoints.CondensedOutput(group))

	golor.Println(`#(bold,cyan)[Deploying {{.Name}}]`, instance.Configuration)
	_, err = instance.Deploy(ctx, &factoryv1.DeploymentRequest{
		Environment:             &basev1.Environment{Name: env.Name},
		DependencyEndpointGroup: group,
	})
	if err != nil {
		return logger.Wrapf(err, "cannot build runtime")
	}
	golor.Println(`#(bold,cyan)[Build {{.Name}}]: #(green)[OK]`, instance.Configuration)
	return nil
}
