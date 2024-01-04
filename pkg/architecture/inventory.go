package architecture

import (
	"context"

	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/cli/pkg/services"
	"github.com/codefly-dev/core/configurations"
	basev1 "github.com/codefly-dev/core/generated/go/base/v1"
)

func LoadProject(ctx context.Context, project *configurations.Project) (*basev1.Project, error) {
	w := wool.Get(ctx).In("overview.LoadProject")
	out := project.Proto()
	apps, err := project.LoadApplications(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "failed to load applications")
	}
	for _, app := range apps {
		a, err := LoadApplication(ctx, app)
		if err != nil {
			return nil, w.Wrapf(err, "failed to load application: %s", app.Name)
		}
		out.Applications = append(out.Applications, a)
	}
	return out, nil
}

func LoadApplication(ctx context.Context, app *configurations.Application) (*basev1.Application, error) {
	w := wool.Get(ctx).In("overview.LoadApplication")
	out := app.Proto()
	svcs, err := app.LoadServices(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "failed to load svcs")
	}
	for _, service := range svcs {
		s, err := LoadService(ctx, service)
		if err != nil {
			return nil, w.Wrapf(err, "failed to load service: %s", service.Name)
		}
		out.Services = append(out.Services, s)
	}
	return out, nil
}

func LoadService(ctx context.Context, service *configurations.Service) (*basev1.Service, error) {
	w := wool.Get(ctx).In("overview.LoadService")
	out := service.Proto()
	// Get endpoints from services
	instance, err := services.Load(ctx, service)
	if err != nil {
		return nil, w.Wrapf(err, "failed to load service: %s", service.Name)
	}
	init, err := instance.Runtime.Load(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "failed to init service: %s", service.Name)
	}
	out.Agent = service.Agent.Proto()
	out.Endpoints = init.Endpoints
	return out, nil
}
