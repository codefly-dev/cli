package application

import (
	"github.com/codefly-dev/cli/pkg/plugins/services"
	servicev1 "github.com/codefly-dev/cli/proto/v1/services"
	runtimev1 "github.com/codefly-dev/cli/proto/v1/services/runtime"
	"github.com/codefly-dev/core/shared"
)

// FactoryInit the application components
func (app *Application) FactoryInit() error {
	logger := shared.NewLogger("applications.Init<%s>", app.Configuration.Name)
	for _, service := range app.Plan.Services {
		logger.Debugf("init %v", service.Unique())
		app.uniques[service.Configuration.Unique()] = service
		err := app.FactoryInitService(service)
		if err != nil {
			return logger.Wrapf(err, "cannot init service")
		}
	}
	return nil
}

// RuntimeInit the application components
func (app *Application) RuntimeInit() error {
	logger := shared.NewLogger("applications.Init<%s>", app.Configuration.Name)
	for _, service := range app.Plan.Services {
		logger.Debugf("init %v", service.Unique())
		app.uniques[service.Configuration.Unique()] = service
		err := app.RuntimeInitService(service)
		if err != nil {
			return logger.Wrapf(err, "cannot init service")
		}
	}
	return nil
}

func (app *Application) FactoryInitService(instance *services.Instance) error {
	logger := shared.NewLogger("applications.FactoryInitService<%s::%v>", instance.Configuration.Name, instance.Configuration.Plugin.Identifier)
	if instance.Initialized {
		return nil
	}

	group, err := GetEndpointDependencyGroup(instance.Configuration)
	if err != nil {
		return logger.Wrapf(err, "cannot get application group endpoint")
	}

	logger.DebugMe("group: %v", CondensedOutput(group))
	ShowEndpointManagerState()
	req := &servicev1.InitRequest{
		Debug:                   shared.Debug(),
		Location:                instance.Location,
		Identity:                instance.ServiceIdentity,
		DependencyEndpointGroup: group,
	}

	init, err := instance.FactoryInit(req)
	if err != nil {
		return logger.Wrapf(err, "cannot init factory")
	}

	logger.Debugf("response: version: %v, #endpoints: %d, #channels: %d", init.Version, len(init.Endpoints), len(init.Channels))

	err = app.EndpointManager.Add(instance.Configuration, init.Endpoints)
	if err != nil {
		return logger.Wrapf(err, "cannot add endpoints")
	}
	instance.Initialized = true
	return nil
}

func (app *Application) RuntimeInitService(instance *services.Instance) error {
	logger := shared.NewLogger("applications.InitService<%s::%v>", instance.Configuration.Name, instance.Configuration.Plugin.Identifier)
	if instance.Initialized {
		return nil
	}
	group, err := GetEndpointDependencyGroup(instance.Configuration)
	if err != nil {
		return logger.Wrapf(err, "cannot get application group endpoint")
	}

	req := &servicev1.InitRequest{
		Debug:                   shared.Debug(),
		Location:                instance.Location,
		Identity:                instance.ServiceIdentity,
		DependencyEndpointGroup: group,
	}

	init, err := instance.RuntimeInit(req)
	if err != nil {
		logger.Debugf("ERROR: %v", err)
		return logger.Wrapf(err, "cannot init: something dramatic happened")
	}

	if init.Status.State == runtimev1.InitStatus_ERROR {
		return logger.Errorf("cannot init service: %v", init.Status.Message)
	}

	logger.Tracef("init response: version: %v, #endpoints: %d, #channels: %d", init.Version, len(init.Endpoints), len(init.Channels))

	err = app.EndpointManager.Add(instance.Configuration, init.Endpoints)
	if err != nil {
		return logger.Wrapf(err, "cannot add endpoints")
	}
	instance.Initialized = true
	return nil
}
