package imports

type ModuleImporter interface {
	NewModuleName() string
	WorkspaceName() string
	Source() SourceImporter

	Fetch() error
}

type ServiceImporter interface {
	Fetch(rec *Recommendation) error
}

type Importer struct {
	ModuleImporter
	ServiceImporter
}

func ImportModule(imp *Importer) error {
	//logger := shared.GetBaseLogger(ctx).With("import.ImportModule")
	//logger.Debuf("Importing modules to <%s>", imp.WorkspaceName())
	//err := imp.ModuleImporter.Fetch()
	//if err != nil {
	//	return err
	//}
	//
	//recommendation, err := imp.ModuleImporter.Source().Analyze()
	//if err != nil {
	//	return logger.Wrapf(err, "cannot analyze source")
	//}
	//
	//err = imp.ServiceImporter.Fetch(recommendation)
	//if err != nil {
	//	return logger.Wrapf(err, "cannot fetch base")
	//}
	//
	//if err != nil {
	//	return err
	//}
	//workspace, err := resources.LoadWorkspaceFromName(imp.WorkspaceName())
	//if err != nil {
	//	return logger.Wrapf(err, "needs a workspace to import into")
	//}
	//resources.Global().SetCurrentWorkspace(workspace)
	//
	//// Will clone to the name
	//name := imp.NewModuleName()
	//
	//conf, err := resources.NewModule(name)
	//if err != nil {
	//	return logger.Wrapf(err, "cannot create modules")
	//}
	//
	//err = conf.Save()
	//if err != nil {
	//	return logger.Wrapf(err, "cannot save modules")
	//}
	//resources.SetCurrentModule(conf)
	//if err != nil {
	//	return logger.Wrapf(err, "cannot analyze modules")
	//}
	//
	//inputs, err := imp.ServiceImporter.CreationInputs()
	//if err != nil {
	//	return logger.Wrapf(err, "cannot get base agent")
	//}
	//for _, input := range inputs {
	//	logger.Debuf("creating service <%s>", input.Name)
	//	err := services.Add(input)
	//	if err != nil {
	//		return logger.Wrapf(err, "cannot create service")
	//	}
	//}
	//
	//// Load an sync
	//golor.Println(`#(white,bold)[We are syncing the modules].`)
	//app, err := module.Load(workspace, conf, module.BuilderMode)
	//if err != nil {
	//	return logger.Wrapf(err, "cannot load modules")
	//}
	//err = app.Sync(context.Background())
	//if err != nil {
	//	return logger.Wrapf(err, "cannot sync modules")
	//}

	return nil
}

func ValidateRepository(repository string) error {
	// Check valid git repository
	// reg := regexp.Compile(`^(https:\/\/github\.com\/|git@github\.com:)([a-zA-Z0-9-]+)\/([a-zA-Z0-9-]+)$`)
	return nil
}
