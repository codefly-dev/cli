package services

import (
	"fmt"
	"path"

	"github.com/codefly-dev/cli/pkg/plugins/endpoints"

	"github.com/codefly-dev/cli/pkg/cli/display"
	prompt "github.com/codefly-dev/cli/pkg/cli/prompts/services"
	v1 "github.com/codefly-dev/cli/proto/v1/services"
	factoryv1 "github.com/codefly-dev/cli/proto/v1/services/factory"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
)

type CreationInput struct {
	Name      string
	Namespace string
	Plugin    *configurations.Plugin
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

func Add(input *CreationInput, opts ...configurations.Option) error {
	scope := configurations.WithScope(opts...)
	logger := shared.NewLogger("services.Creator<%s>.Add", scope.Application.Name)
	logger.Debugf("adding from %v", input.Plugin)

	// Instance configuration
	svc, err := configurations.NewService(input.Name, input.Namespace, input.Plugin, opts...)
	if err != nil {
		return logger.Errorf("cannot create svc <%s>: %v", input.Name, err)
	}

	svc.Domain = scope.Application.ServiceDomain(input.Name)
	svc.Version = "0.0.0"

	entry, err := svc.Reference()
	if err != nil {
		return logger.Wrapf(err, "cannot get svc entry")
	}
	destination := path.Join(scope.Application.Dir(), entry.RelativePath())

	if shared.DirectoryExists(destination) {
		override, err := prompt.Override()
		if err != nil {
			return logger.Wrapf(err, "cannot ask for override")
		}
		if !override {
			display.DestinationExists(display.DestinationExistsMessage{Destination: destination})
			return nil
		}
	}
	err = svc.Save()
	if err != nil {
		return logger.Wrapf(err, "cannot save svc configuration <%s>", svc.Name)
	}

	instance, err := NewServiceInstance(svc, scope.Application)
	if err != nil {
		return logger.Wrapf(err, "cannot instantiate service")
	}

	err = instance.LoadFactory()
	if err != nil {
		return logger.Errorf("cannot load factory for plugin <%s>: %v", input.Plugin.Name(), err)
	}

	err = shared.CheckDirectoryOrCreate(destination)
	if err != nil {
		return logger.Wrapf(err, "cannot create directory")
	}

	init, err := instance.FactoryInit(&v1.InitRequest{
		Debug:    shared.Debug(),
		Location: destination,
		Identity: &v1.ServiceIdentity{
			Name:      svc.Name,
			Domain:    svc.Domain,
			Namespace: svc.Namespace,
		},
	})
	if err != nil {
		return logger.Wrapf(err, "FactoryInit for plugin <%s>", input.Plugin.Name())
	}
	logger.Debugf("creating svc <%s::%s> from plugin <%s>", input.Name, init.Version.Version, input.Plugin.Name())

	create, err := instance.Create(&factoryv1.CreateRequest{})
	if err != nil {
		return logger.Errorf("cannot run create request for plugin <%s>: %v", input.Plugin.Name(), err)
	}

	// Reload the configuration
	svc, err = configurations.LoadServiceFromDir(destination)
	if err != nil {
		return logger.Errorf("cannot reload svc configuration <%s>: %v", input.Name, err)
	}
	// Update endpoints
	for _, endpoint := range create.Endpoints {
		e, err := endpoints.FromProtoEndpoint(endpoint)
		if err != nil {
			return logger.Wrapf(err, "cannot convert endpoint")
		}
		golor.Println(`#(bold,blue)[🚀Adding endpoint <{{.Name}}>]`, e)
		svc.Endpoints = append(svc.Endpoints, e)
	}

	// Copy
	for _, file := range input.Files {
		err = shared.CopyFile(file.Path, path.Join(destination, file.Name))
		if err != nil {
			return logger.Wrapf(err, "cannot copy file")
		}
	}

	golor.Println(`#(bold,blue)[🚀Successfully created svc {{.Name}} from plugin {{.Plugin}}]`,
		map[string]string{"Name": input.Name, "Plugin": input.Plugin.Name()})

	// Map svc to applications configuration when there are no dependencies
	if len(input.RequiredBy) == 0 {
		err = configurations.MustCurrentApplication().AddService(svc)
		if err != nil {
			return logger.Errorf("cannot add svc to applications configuration: %v", err)
		}
		golor.Println(`#(bold,blue)[🚀Successfully added svc {{.Name}} to current application]`, svc)
	}

	requiredBy, err := configurations.LoadServicesFromInput(input.RequiredBy...)
	if err != nil {
		return logger.Errorf("cannot load required services: %v", err)
	}
	// Something that is required is a dependency
	for _, dependency := range requiredBy {
		logger.Debugf("adding dependency for <%s>", dependency.Name)
		err := dependency.AddDependencyReference(svc)
		if err != nil {
			return logger.Errorf("cannot add dependency to svc <%s>: %v", dependency.Name, err)
		}
		err = dependency.Save()
		if err != nil {
			return logger.Errorf("cannot save svc configuration <%s>: %v", dependency.Name, err)
		}
		golor.Println(`#(bold,blue)[🚀Adding svc dependency <{{.Name}}>]`, dependency)
	}
	// Something on which we depend is a requirement
	dependsOn, err := configurations.LoadServicesFromInput(input.DependsOn...)
	if err != nil {
		return logger.Errorf("cannot load required services: %v", err)
	}
	for _, requirement := range dependsOn {
		err := AddDependencyWithClient(svc, requirement, input.WithClientDecider)
		if err != nil {
			return logger.Wrapf(err, "cannot add dependency to svc <%s>", svc.Name)
		}
	}

	err = scope.Application.Save()
	if err != nil {
		return logger.Errorf("cannot save application configuration: %v", err)
	}

	// We are done adding requirements to svc
	err = svc.Save()
	if err != nil {
		return logger.Wrapf(err, "cannot save svc configuration <%s>", svc.Name)
	}

	return nil
}

func AddDependencyWithClient(service *configurations.Service, requirement *configurations.Service, input WithClientDecider) error {
	// logger := shared.NewLogger("services.AddDependencyWithClient<%s> <<< <%s>", service.Name, requirement.Name)
	if input == nil {
		return nil
	}
	var uses []*configurations.EndpointReference
	for _, endpoint := range requirement.Endpoints {
		// We create a Decider for each endpoint
		decider := input.Get(requirement, endpoint)
		err := decider.Fetch()
		if err != nil {
			return fmt.Errorf("cannot fetch client decider: %v", err)
		}
		client, err := decider.Includes()
		if err != nil {
			return fmt.Errorf("cannot get client: %v", err)
		}
		if client == nil {
			continue
		}
		//uses = append(uses, client.Endpoint.Unique())
	}
	service.Dependencies = append(service.Dependencies,
		&configurations.ServiceDependency{
			Name:                 requirement.Name,
			Application:          requirement.Application,
			RelativePathOverride: requirement.RelativePathOverride,
			Endpoints:            uses,
		})
	return nil
}
