package application

import (
	"time"

	"github.com/codefly-dev/cli/pkg/services"
	factoryv1 "github.com/codefly-dev/core/proto/v1/go/services/factory"

	"github.com/briandowns/spinner"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
)

func (app *Application) Deploy() error {
	logger := shared.NewLogger("applications.Deploy<%s>", app.Configuration.Name)
	for _, service := range app.Plan.Services {
		err := app.DeployService(service)
		if err != nil {
			return logger.Wrapf(err, "cannot build service <%s>", service.Configuration.Name)
		}
	}
	return nil
}

func (app *Application) DeployService(service *services.Instance) error {
	logger := shared.NewLogger("applications.DeployService<%s>", service.Configuration.Name)
	if service.Runtime == nil {
		return logger.Errorf("runtime for service <%s> is not initialized, run first app.Init()", service.Configuration.Name)
	}
	logger.TODO("Type of build will depend on deployment, right now assume we dockerize")
	// What kind of build will be picked from the deployment
	golor.Println(`#(bold,cyan)[Deploying {{.Name}}]`, service.Configuration)
	s := spinner.New(spinner.CharSets[9], 100*time.Millisecond) // Build our new spinner
	s.Start()
	_, err := service.Deploy(&factoryv1.DeploymentRequest{})
	if err != nil {
		return logger.Wrapf(err, "cannot build runtime")
	}
	s.Stop()
	golor.Println(`#(bold,cyan)[Build {{.Name}}]: #(green)[OK]`, service.Configuration)
	return nil
}
