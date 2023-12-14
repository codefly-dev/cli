package application

import (
	"context"
	"time"

	"github.com/codefly-dev/core/agents/services"

	services2 "github.com/codefly-dev/cli/pkg/services"
	factoryv1 "github.com/codefly-dev/core/generated/v1/go/proto/services/factory"

	"github.com/briandowns/spinner"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
)

func (app *Application) Update(ctx context.Context) error {
	logger := shared.GetLogger(ctx).With("applications.Update<%s>", app.Configuration.Name)
	for _, service := range app.Plan.Services {
		err := app.UpdateService(ctx, service)
		if err != nil {
			return logger.Wrapf(err, "cannot build service <%s>", service.Configuration.Name)
		}
	}
	return nil
}

func (app *Application) UpdateService(ctx context.Context, service *services2.Instance) error {
	logger := shared.GetLogger(ctx).With("applications.UpdateService<%s>", service.Configuration.Name)
	golor.Println(`#(bold,cyan)[Updating {{.Name}}]`, service.Configuration)
	s := spinner.New(spinner.CharSets[9], 100*time.Millisecond) // Build our new spinner
	s.Start()
	var err error
	service.Factory, err = services.LoadFactory(ctx, service.Configuration)
	if err != nil {
		return logger.Wrapf(err, "cannot load factory")
	}
	_, err = service.Update(ctx, &factoryv1.UpdateRequest{})
	if err != nil {
		return logger.Wrapf(err, "cannot update")
	}
	s.Stop()
	golor.Println(`#(bold,cyan)[Updated {{.Name}}]: #(green)[OK]`, service.Configuration)
	return nil
}
