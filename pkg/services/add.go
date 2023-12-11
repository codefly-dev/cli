package services

import (
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

type CreationInput struct {
	Name      string
	Namespace string
	Agent     *configurations.Agent
	// Services

	RequiredBy []string

	// Dependencies
	DependsOn         []string
	WithClientDecider WithClientDecider // Decide what clients we include for dependencies

	// Files
	Files []shared.CopyInstruction

	// We want a view of the application
	Application *configurations.Application
}

func (input *CreationInput) SetNamespace(namespace string) error {
	// TODO: Validation
	input.Namespace = namespace
	return nil
}

type Client struct {
	Endpoint *configurations.Endpoint
}

type ClientDecider interface {
	Includes() (*Client, error)
	Fetch() error
}

type WithClientDecider interface {
	Get(dependency *configurations.Service, endpoint *configurations.Endpoint) ClientDecider
}

func Add(input *CreationInput) error {
	return nil
}

//	ctx := context.Background()
//	logger := shared.GetLogger(ctx).With("services.Creator<%s>.Add", scope.Application.Name)
//	logger.Debugf("adding from %v", input.Agent)
//
//	// Instance configuration
//	svc, err := configurations.NewService(input.Name, input.Namespace, input.Agent, opts...)
//	if err != nil {
//		return logger.Errorf("cannot create svc <%s>: %v", input.Name, err)
//	}
//
//	svc.Domain = scope.Application.ServiceDomain(input.Name)
//	svc.Version = "0.0.0"
//
//	entry, err := svc.Reference()
//	if err != nil {
//		return logger.Wrapf(err, "cannot get svc entry")
//	}
//	destination := path.Join(scope.Application.Dir(), entry.RelativePath())
//
//	if shared.DirectoryExists(destination) {
//		override, err := prompt.Override()
//		if err != nil {
//			return logger.Wrapf(err, "cannot ask for override")
//		}
//		if !override {
//			display.DestinationExists(display.DestinationExistsMessage{Destination: destination})
//			return nil
//		}
//	}
//	err = svc.Save()
//	if err != nil {
//		return logger.Wrapf(err, "cannot save svc configuration <%s>", svc.Name)
//	}
//
//	instance, err := NewServiceInstance(svc, scope.Application)
//	if err != nil {
//		return logger.Wrapf(err, "cannot instantiate service")
//	}
//
//	err = instance.LoadFactory()
//	if err != nil {
//		return logger.Errorf("cannot load factory for agent <%s>: %v", input.Agent.Name(), err)
//	}
//
//	err = shared.CheckDirectoryOrCreate(destination)
//	if err != nil {
//		return logger.Wrapf(err, "cannot create directory")
//	}
//
//	init, err := instance.FactoryInit(&v1.InitRequest{
//		Debug:    shared.IsDebug(),
//		Location: destination,
//		Identity: &v1.ServiceIdentity{
//			Name:        svc.Name,
//			Application: scope.Application.Name,
//			Domain:      svc.Domain,
//			Namespace:   svc.Namespace,
//		},
//	})
//	if err != nil {
//		return logger.Wrapf(err, "FactoryInit for agent <%s>", input.Agent.Name())
//	}
//	logger.Debugf("creating svc <%s::%s> from agent <%s>", input.Name, init.Version.Version, input.Agent.Name())
//
//	// README
//	rendered, err := glamour.Render(init.ReadMe, "dark")
//	if err != nil {
//		return logger.Errorf("cannot render readme: %v", err)
//	}
//
//	// Print the rendered markdown to the console
//	fmt.Println(rendered)
//
//	create, err := instance.Create(&factoryv1.CreateRequest{})
//	if err != nil {
//		return logger.Errorf("cannot run create request for agent <%s>: %v", input.Agent.Name(), err)
//	}
//
//	// Reload the configuration
//	svc, err = configurations.LoadServiceFromDir(destination)
//	if err != nil {
//		return logger.Errorf("cannot reload svc configuration <%s>: %v", input.Name, err)
//	}
//	// Update endpoints
//	for _, endpoint := range create.Endpoints {
//		e, err := endpoints.FromProtoEndpoint(endpoint)
//		if err != nil {
//			return logger.Wrapf(err, "cannot convert endpoint")
//		}
//		golor.Println(`#(bold,blue)[🚀Adding endpoint <{{.Name}}>]`, e)
//		svc.Endpoints = append(svc.Endpoints, e)
//	}
//
//	// Copy
//	for _, file := range input.Files {
//		err = shared.CopyFile(file.Path, path.Join(destination, file.Name))
//		if err != nil {
//			return logger.Wrapf(err, "cannot copy file")
//		}
//	}
//
//	golor.Println(`#(bold,blue)[🚀Successfully created svc {{.Name}} from agent {{.Agent}}]`,
//		map[string]string{"Name": input.Name, "Agent": input.Agent.Name()})
//
//	// Map svc to applications configuration when there are no dependencies
//	if len(input.RequiredBy) == 0 {
//		err = scope.Application.AddService(svc)
//		if err != nil {
//			return logger.Errorf("cannot add svc to applications configuration: %v", err)
//		}
//		golor.Println(`#(bold,blue)[🚀Successfully added svc {{.Name}} to current application]`, svc)
//	}
//
//	requiredBy, err := configurations.LoadServicesFromInput(input.RequiredBy...)
//	if err != nil {
//		return logger.Errorf("cannot load required services: %v", err)
//	}
//	// Something that is required is a dependency
//	for _, dependency := range requiredBy {
//		logger.Debugf("adding dependency for <%s>", dependency.Name)
//		err := dependency.AddDependencyReference(svc)
//		if err != nil {
//			return logger.Errorf("cannot add dependency to svc <%s>: %v", dependency.Name, err)
//		}
//		err = dependency.Save()
//		if err != nil {
//			return logger.Errorf("cannot save svc configuration <%s>: %v", dependency.Name, err)
//		}
//		golor.Println(`#(bold,blue)[🚀Adding svc dependency <{{.Name}}>]`, dependency)
//	}
//	// Something on which we depend is a requirement
//	dependsOn, err := configurations.LoadServicesFromInput(input.DependsOn...)
//	if err != nil {
//		return logger.Errorf("cannot load required services: %v", err)
//	}
//	for _, requirement := range dependsOn {
//		err := AddDependencyWithClient(svc, requirement, input.WithClientDecider)
//		if err != nil {
//			return logger.Wrapf(err, "cannot add dependency to svc <%s>", svc.Name)
//		}
//	}
//
//	err = scope.Application.Save(ctx)
//	if err != nil {
//		return logger.Errorf("cannot save application configuration: %v", err)
//	}
//
//	// We are done adding requirements to svc
//	err = svc.Save()
//	if err != nil {
//		return logger.Wrapf(err, "cannot save svc configuration <%s>", svc.Name)
//	}
//
//	return nil
//}
