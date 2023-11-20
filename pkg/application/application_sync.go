package application

import (
	"context"

	"github.com/codefly-dev/cli/pkg/plugins/services"
	factoryv1 "github.com/codefly-dev/cli/proto/v1/services/factory"
	"github.com/codefly-dev/core/shared"
)

func (app *Application) Sync(ctx context.Context) error {
	logger := shared.NewLogger("applications.Sync<%s>", app.Configuration.Name)
	logger.Debugf("current")
	for _, service := range app.Plan.Services {
		err := app.SyncService(ctx, service)
		if err != nil {
			return logger.Wrapf(err, "cannot sync service <%s>", service.Configuration.Name)
		}
	}
	return nil
}

func (app *Application) SyncService(ctx context.Context, instance *services.Instance) error {
	logger := shared.NewLogger("applications.SyncService<%s>", instance.Configuration.Name)
	if instance.Runtime == nil {
		return logger.Errorf("runtime for instance <%s> is not initialized, run first app.Init()", instance.Configuration.Name)
	}

	group, err := GetEndpointDependencyGroup(instance.Configuration)
	if err != nil {
		return logger.Wrapf(err, "cannot get application group endpoints")
	}

	logger.Debugf("dependency group: %v", CondensedOutput(group))

	sync, err := instance.Sync(&factoryv1.SyncRequest{DependencyEndpointGroup: group})
	if err != nil {
		return logger.Wrapf(err, "cannot sync runtime")
	}
	logger.Tracef("sync response: %v", sync)
	return nil
}
