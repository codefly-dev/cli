package architecture

import (
	"context"

	"github.com/codefly-dev/cli/pkg/services/services"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"

	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
)

func LoadWorkspace(ctx context.Context, workspace *resources.Workspace) (*basev0.Workspace, error) {
	w := wool.Get(ctx).In("overview.LoadWorkspace")
	out, err := workspace.Proto()
	if err != nil {
		return nil, w.Wrapf(err, "failed to load workspace")
	}
	mods, err := workspace.LoadModules(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "failed to load modules")
	}
	for _, mod := range mods {
		a, err := LoadModule(ctx, workspace, mod)
		if err != nil {
			return nil, w.Wrapf(err, "failed to load module: %s", mod.Name)
		}
		out.Modules = append(out.Modules, a)
	}
	return out, nil
}

func LoadModule(ctx context.Context, workspace *resources.Workspace, mod *resources.Module) (*basev0.Module, error) {
	w := wool.Get(ctx).In("overview.LoadModule")
	out := mod.Proto()
	svcs, err := mod.LoadServices(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "failed to load svcs")
	}
	for _, service := range svcs {
		s, err := LoadService(ctx, workspace, service)
		if err != nil {
			return nil, w.Wrapf(err, "failed to load service: %s", service.Name)
		}
		out.Services = append(out.Services, s)
	}
	return out, nil
}

func LoadService(ctx context.Context, workspace *resources.Workspace, service *resources.Service) (*basev0.Service, error) {
	w := wool.Get(ctx).In("overview.LoadService")
	out := service.Proto()
	// Get endpoints from services
	instance, err := services.Load(ctx, service)
	if err != nil {
		return nil, w.Wrapf(err, "failed to load service: %s", service.Name)
	}

	instance.Workspace = workspace
	err = instance.LoadRuntime(ctx, false)
	if err != nil {
		return nil, w.Wrapf(err, "failed to load service runtime: %s", service.Name)
	}

	init, err := instance.Runtime.Load(ctx, shared.Must(resources.LocalEnvironment().Proto()))
	if err != nil {
		return nil, w.Wrapf(err, "failed to init service: %s", service.Name)
	}

	out.Agent = service.Agent.Proto()
	w.Debug("loaded", wool.Field("endpoints", resources.MakeManyEndpointSummary(init.Endpoints)))
	out.Endpoints = init.Endpoints

	for _, dep := range service.ServiceDependencies {
		out.ServiceDependencies = append(out.ServiceDependencies, &basev0.ServiceReference{Name: dep.Name, Module: dep.Module})
	}

	return out, nil
}
