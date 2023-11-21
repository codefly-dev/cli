package imports

import (
	"context"

	"github.com/codefly-dev/cli/pkg/application"
	"github.com/codefly-dev/cli/pkg/plugins/services"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
)

type ApplicationImporter interface {
	NewApplicationName() string
	ProjectName() string
	Source() SourceImporter

	Fetch() error
}

type ServiceImporter interface {
	Fetch(rec *Recommendation) error
	CreationInputs() ([]*services.CreationInput, error)
}

type Importer struct {
	ApplicationImporter
	ServiceImporter
}

func ImportApplication(imp *Importer) error {
	logger := shared.NewLogger("import.ImportApplication")
	logger.Debugf("Importing applications to <%s>", imp.ProjectName())
	err := imp.ApplicationImporter.Fetch()
	if err != nil {
		return err
	}

	recommendation, err := imp.ApplicationImporter.Source().Analyze()
	if err != nil {
		return logger.Wrapf(err, "cannot analyze source")
	}

	err = imp.ServiceImporter.Fetch(recommendation)
	if err != nil {
		return logger.Wrapf(err, "cannot fetch base")
	}

	if err != nil {
		return err
	}
	project, err := configurations.LoadProjectFromName(imp.ProjectName())
	if err != nil {
		return logger.Wrapf(err, "needs a project to import into")
	}
	configurations.SetCurrentProject(project)

	// Will clone to the name
	name := imp.NewApplicationName()

	conf, err := configurations.NewApplication(name)
	if err != nil {
		return logger.Wrapf(err, "cannot create applications")
	}

	err = conf.Save()
	if err != nil {
		return logger.Wrapf(err, "cannot save applications")
	}
	configurations.SetCurrentApplication(conf)
	if err != nil {
		return logger.Wrapf(err, "cannot analyze applications")
	}

	inputs, err := imp.ServiceImporter.CreationInputs()
	if err != nil {
		return logger.Wrapf(err, "cannot get base plugin")
	}
	for _, input := range inputs {
		logger.Debugf("creating service <%s>", input.Name)
		err := services.Add(input)
		if err != nil {
			return logger.Wrapf(err, "cannot create service")
		}
	}

	// Load an sync
	golor.Println(`#(white,bold)[We are syncing the applications].`)
	app, err := application.Load(project, conf, application.FactoryMode)
	if err != nil {
		return logger.Wrapf(err, "cannot load applications")
	}
	err = app.Sync(context.Background())
	if err != nil {
		return logger.Wrapf(err, "cannot sync applications")
	}

	return nil
}

func ValidateRepository(repository string) error {
	// Check valid git repository
	// reg := regexp.Compile(`^(https:\/\/github\.com\/|git@github\.com:)([a-zA-Z0-9-]+)\/([a-zA-Z0-9-]+)$`)
	return nil
}
