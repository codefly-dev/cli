package application

import (
	"context"

	"github.com/codefly-dev/cli/pkg/services"
	"github.com/codefly-dev/core/wool"
)

// FactoryInit the application components
func (app *Application) FactoryInit(ctx context.Context) error {
	w := wool.Get(ctx).In("applications.Init<%s>", app.Configuration.Name)
	for _, service := range app.Plan.Services {
		logger.Debugf("init %v", service.Unique())
		app.uniques[service.Configuration.Unique()] = service
		err := app.FactoryInitService(ctx, service)
		if err != nil {
			return logger.Wrapf(err, "cannot init service")
		}
	}
	return nil
}

// RuntimeInit the application components
func (app *Application) RuntimeInit(ctx context.Context) error {
	w := wool.Get(ctx).In("applications.Init<%s>", app.Configuration.Name)
	for _, service := range app.Plan.Services {
		logger.Debugf("init %v", service.Unique())
		app.uniques[service.Configuration.Unique()] = service
		err := app.RuntimeInitService(ctx, service)
		if err != nil {
			return logger.Wrapf(err, "cannot init service")
		}
	}
	return nil
}

func (app *Application) FactoryInitService(ctx context.Context, instance *services.Instance) error {
	//w := wool.Get(ctx).In("applications.FactoryInitService<%s::%v>", instance.Configuration.Name, instance.Configuration.Agent.Identifier)
	//if instance.Initialized {
	//	return nil
	//}
	//
	//group, err := GetEndpointDependencyGroup(ctx, instance.Configuration)
	//if err != nil {
	//	return logger.Wrapf(err, "cannot get application group endpoint")
	//}
	//
	//logger.Debugf("group: %v", endpoints.CondensedOutput(group))
	//ShowEndpointManagerState(ctx)
	//req := &factoryv1.InitRequest{
	//	Debug:                   shared.IsDebug(),
	//	Location:                instance.Location,
	//	Identity:                instance.ServiceIdentity,
	//	DependencyEndpointGroup: group,
	//}
	//
	//init, err := instance.FactoryInit(ctx, req)
	//if err != nil {
	//	return logger.Wrapf(err, "cannot init factory")
	//}
	//
	//err = app.EndpointManager.Add(ctx, instance.Configuration, init.Endpoints)
	//if err != nil {
	//	return logger.Wrapf(err, "cannot add endpoints")
	//}
	//instance.Initialized = true
	return nil
}

func (app *Application) RuntimeInitService(ctx context.Context, instance *services.Instance) error {
	//w := wool.Get(ctx).In("applications.InitService<%s::%v>", instance.Configuration.Name, instance.Configuration.Agent.Identifier)
	//if instance.Initialized {
	//	return nil
	//}
	//group, err := GetEndpointDependencyGroup(ctx, instance.Configuration)
	//if err != nil {
	//	return logger.Wrapf(err, "cannot get application group endpoint")
	//}
	//
	//req := &runtimev1.InitRequest{
	//	Debug:                   shared.IsDebug(),
	//	Location:                instance.Location,
	//	Identity:                instance.ServiceIdentity,
	//	DependencyEndpointGroup: group,
	//}
	//
	//init, err := instance.RuntimeInit(ctx, req)
	//if err != nil {
	//	logger.Debugf("ERROR: %v", err)
	//	return logger.Wrapf(err, "cannot init: something dramatic happened")
	//}
	//
	//if init.Status.State == agents.InitError {
	//	return logger.Errorf("cannot init service: %v", init.Status.Message)
	//}
	//
	//err = app.EndpointManager.Add(ctx, instance.Configuration, init.Endpoints)
	//if err != nil {
	//	return logger.Wrapf(err, "cannot add endpoints")
	//}
	//instance.Initialized = true
	return nil
}
