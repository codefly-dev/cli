package application

import (
	"context"

	"github.com/codefly-dev/cli/pkg/services"
	runtimev1 "github.com/codefly-dev/core/generated/v1/go/proto/services/runtime"

	"github.com/codefly-dev/core/shared"
)

// Run is a blocking call to run the applications
func (app *Application) Run(ctx context.Context) error {
	logger := shared.GetLogger(ctx).With("applications.Run<%s>", app.Configuration.Name)
	for _, service := range app.Plan.Services {
		logger.Debugf("starting service %v", service.Configuration.Name)
		err := app.StartService(ctx, service)
		if err != nil {
			return logger.Wrapf(err, "cannot start service <%s>", service.Configuration.Name)
		}
	}
	// Wait for the context to be done
	<-ctx.Done()
	return nil
}

// StartService starts the service in a non-blocking way
// Response has the tracker: this is how we detect re-start
func (app *Application) StartService(ctx context.Context, instance *services.Instance) error {
	logger := shared.GetLogger(ctx).With("applications.StartService<%s>", instance.Configuration.Name)

	if !instance.Ready {
		return logger.Errorf("service is not ready")
	}

	// What are the dependencies
	mappings, err := NetworkMappingsFor(ctx, instance.Configuration.Dependencies)
	if err != nil {
		return logger.Wrapf(err, "cannot get network mappings")
	}

	logger.Debugf("network mappings #%d", len(mappings))

	start, err := instance.Start(ctx, &runtimev1.StartRequest{
		NetworkMappings: mappings,
	})
	if err != nil {
		return logger.Wrapf(err, "cannot start runtime")
	}

	if start.Status.State != runtimev1.StartStatus_STARTED {
		return logger.Errorf("cannot start service: %v", start.Status.Message)
	}

	logger.Tracef("start response: %v", start)
	instance.Started = true

	err = app.ServiceTracker.Track(ctx, instance.Configuration, instance.Runtime, start.Trackers)
	if err != nil {
		return logger.Wrapf(err, "cannot track instance")
	}

	return nil
}
