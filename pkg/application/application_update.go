package application

import (
	"time"

	"github.com/briandowns/spinner"
	"github.com/codefly-dev/cli/pkg/plugins/services"
	factoryv1 "github.com/codefly-dev/cli/proto/v1/services/factory"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
)

func (app *Application) Update() error {
	logger := shared.NewLogger("applications.Update<%s>", app.Configuration.Name)
	for _, service := range app.Plan.Services {
		err := app.UpdateService(service)
		if err != nil {
			return logger.Wrapf(err, "cannot build service <%s>", service.Configuration.Name)
		}
	}
	return nil
}

func (app *Application) UpdateService(service *services.Instance) error {
	logger := shared.NewLogger("applications.UpdateService<%s>", service.Configuration.Name)
	golor.Println(`#(bold,cyan)[Updating {{.Name}}]`, service.Configuration)
	s := spinner.New(spinner.CharSets[9], 100*time.Millisecond) // Build our new spinner
	s.Start()
	var err error
	service.Factory, err = services.LoadFactory(service.Configuration)
	if err != nil {
		return logger.Wrapf(err, "cannot load factory")
	}
	_, err = service.Update(&factoryv1.UpdateRequest{})
	if err != nil {
		return logger.Wrapf(err, "cannot update")
	}
	s.Stop()
	golor.Println(`#(bold,cyan)[Updated {{.Name}}]: #(green)[OK]`, service.Configuration)
	return nil
}
