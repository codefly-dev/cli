package application

import (
	"context"

	"github.com/codefly-dev/cli/pkg/services"
	"github.com/codefly-dev/core/agents/endpoints"
	factoryv1 "github.com/codefly-dev/core/generated/go/services/factory/v1"
	"github.com/codefly-dev/core/wool"
)

func (app *Application) Sync(ctx context.Context) error {
	w := wool.Get(ctx).In("applications.Sync<%s>", app.Configuration.Name)
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
	w := wool.Get(ctx).In("applications.SyncService<%s>", instance.Configuration.Name)
	if instance.Runtime == nil {
		return logger.Errorf("runtime for instance <%s> is not initialized, run first app.Init()", instance.Configuration.Name)
	}
	logger.Debugf("syncing")

	group, err := GetEndpointDependencyGroup(ctx, instance.Configuration)
	if err != nil {
		return logger.Wrapf(err, "cannot get application group endpoints")
	}

	logger.Debugf("dependency group: %v", endpoints.CondensedOutput(group))

	sync, err := instance.Sync(ctx, &factoryv1.SyncRequest{DependencyEndpointGroup: group})
	if err != nil {
		return logger.Wrapf(err, "cannot sync runtime")
	}
	logger.Tracef("sync response: %v", sync)
	return nil
}
