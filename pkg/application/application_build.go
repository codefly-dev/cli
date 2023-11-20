package application

import (
	"context"
	"time"

	"github.com/briandowns/spinner"
	"github.com/codefly-dev/cli/pkg/plugins/services"
	factoryv1 "github.com/codefly-dev/cli/proto/v1/services/factory"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
)

func (app *Application) Build(ctx context.Context) error {
	logger := shared.NewLogger("applications.Build<%s>", app.Configuration.Name)
	for _, service := range app.Plan.Services {
		err := app.BuildService(ctx, service)
		if err != nil {
			return logger.Wrapf(err, "cannot build service <%s>", service.Configuration.Name)
		}
	}
	return nil
}

func (app *Application) BuildService(ctx context.Context, instance *services.Instance) error {
	logger := shared.NewLogger("applications.BuildService<%s>", instance.Configuration.Name)
	if instance.Factory == nil {
		return logger.Errorf("runtime for instance <%s> is not initialized, run first app.Init()", instance.Configuration.Name)
	}

	group, err := GetEndpointDependencyGroup(instance.Configuration)
	if err != nil {
		return logger.Wrapf(err, "cannot get application group endpoints")
	}

	logger.Debugf("dependency group: %v", CondensedOutput(group))
	logger.TODO("Type of build will depend on deployment, right now assume we dockerize")
	// What kind of build will be picked from the deployment

	golor.Println(`#(bold,cyan)[Building {{.Name}}]`, instance.Configuration)
	s := spinner.New(spinner.CharSets[9], 100*time.Millisecond) // Build our new spinner
	s.Start()                                                   // Start the spinner
	_, err = instance.Build(&factoryv1.BuildRequest{
		DependencyEndpointGroup: group,
	})
	if err != nil {
		return logger.Wrapf(err, "cannot build runtime")
	}
	s.Stop()
	golor.Println(`#(bold,cyan)[Build {{.Name}}]: #(green)[OK]`, instance.Configuration)
	return nil
}
