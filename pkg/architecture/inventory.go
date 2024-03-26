package architecture

import (
	"context"

	"github.com/codefly-dev/cli/pkg/services/services"

	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
)

func LoadProject(ctx context.Context, project *configurations.Project) (*basev0.Project, error) {
	w := wool.Get(ctx).In("overview.LoadProject")
	out, err := project.Proto()
	if err != nil {
		return nil, w.Wrapf(err, "failed to load project")
	}
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

func LoadApplication(ctx context.Context, app *configurations.Application) (*basev0.Application, error) {
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

func LoadService(ctx context.Context, service *configurations.Service) (*basev0.Service, error) {
	w := wool.Get(ctx).In("overview.LoadService")
	out := service.Proto()
	// Get endpoints from services
	instance, err := services.Load(ctx, service)
	if err != nil {
		return nil, w.Wrapf(err, "failed to load service: %s", service.Name)
	}

	err = instance.LoadRuntime(ctx, false)
	if err != nil {
		return nil, w.Wrapf(err, "failed to load service: %s", service.Name)
	}

	init, err := instance.Runtime.Load(ctx, shared.Must(configurations.Local().Proto()))
	if err != nil {
		return nil, w.Wrapf(err, "failed to init service: %s", service.Name)
	}

	out.Agent = service.Agent.Proto()
	w.Debug("loaded", wool.Field("endpoints", configurations.MakeManyEndpointSummary(init.Endpoints)))
	out.Endpoints = init.Endpoints

	for _, dep := range service.ServiceDependencies {
		out.ServiceDependencies = append(out.ServiceDependencies, &basev0.ServiceReference{Name: dep.Name, Application: dep.Application})
	}

	return out, nil
}
