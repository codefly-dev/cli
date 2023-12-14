package application

import (
	"context"

	"github.com/codefly-dev/cli/pkg/services"
	runtimev1 "github.com/codefly-dev/core/generated/v1/go/proto/services/runtime"

	"github.com/codefly-dev/core/shared"
)

func (app *Application) Configure(ctx context.Context) error {
	logger := shared.GetLogger(ctx).With("applications.Configure<%s>", app.Configuration.Name)
	if app.IsConfigured {
		return nil
	}
	env, err := NewEnvironment(ctx, app.Configuration.Name)
	if err != nil {
		return logger.Wrapf(err, "cannot create environment")
	}
	app.Environment = env
	for _, service := range app.Plan.Services {
		err = app.ConfigureService(ctx, service)
		if err != nil {
			return logger.Wrapf(err, "cannot configure service")
		}
	}
	app.IsConfigured = true
	return nil
}

func (app *Application) ConfigureService(ctx context.Context, instance *services.Instance) error {
	logger := shared.GetLogger(ctx).With("applications.ConfigureService<%s::%s[%s]>", app.Configuration.Name, instance.Configuration.Name, instance.Configuration.Agent.Identifier)
	logger.Debugf("configuring instance")

	configure, err := instance.Configure(ctx, &runtimev1.ConfigureRequest{})
	if err != nil {
		return logger.Wrapf(err, "something dramatic has happened")
	}
	if configure.Status.State == runtimev1.ConfigureStatus_ERROR {
		return logger.Errorf("cannot configure: %v", configure.Status.Message)
	}

	instance.Ready = true

	logger.Tracef("configure response: %v", configure)
	err = app.Environment.AddNetworkMappings(ctx, instance, configure.NetworkMappings)
	if err != nil {
		return logger.Wrapf(err, "cannot add network mappings")
	}

	instance.Ready = true

	return nil
}
