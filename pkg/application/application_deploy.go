package application

import (
	"github.com/codefly-dev/cli/pkg/services"
	"github.com/codefly-dev/core/configurations"
	basev1 "github.com/codefly-dev/core/proto/v1/go/base"
	factoryv1 "github.com/codefly-dev/core/proto/v1/go/services/factory"

	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
)

func (app *Application) Deploy(env *configurations.Environment) error {
	logger := shared.NewLogger("applications.Deploy<%s>", app.Configuration.Name)
	for _, service := range app.Plan.Services {
		err := app.DeployService(service, env)
		if err != nil {
			return logger.Wrapf(err, "cannot build service <%s>", service.Configuration.Name)
		}
	}
	return nil
}

func (app *Application) DeployService(service *services.Instance, env *configurations.Environment) error {
	logger := shared.NewLogger("applications.DeployService<%s>", service.Configuration.Name)
	if service.Runtime == nil {
		return logger.Errorf("runtime for service <%s> is not initialized, run first app.Init()", service.Configuration.Name)
	}
	logger.TODO("Type of build will depend on deployment, right now assume we dockerize")
	// What kind of build will be picked from the deployment
	golor.Println(`#(bold,cyan)[Deploying {{.Name}}]`, service.Configuration)
	_, err := service.Deploy(&factoryv1.DeploymentRequest{
		Environment: &basev1.Environment{Name: env.Name},
	})
	if err != nil {
		return logger.Wrapf(err, "cannot build runtime")
	}
	golor.Println(`#(bold,cyan)[Build {{.Name}}]: #(green)[OK]`, service.Configuration)
	return nil
}
