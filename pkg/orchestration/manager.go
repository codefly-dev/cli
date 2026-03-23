package orchestration

import (
	"context"
	"fmt"

	"github.com/codefly-dev/core/services"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/wool"
)

type Mode string

const (
	RunMode    Mode = "run"
	TestMode   Mode = "test"
	SyncMode   Mode = "sync"
	BuildMode  Mode = "build"
	DeployMode Mode = "deploy"
)

type IManager interface {
	Unique() string

	// Builder interface

	BuilderDoInit(ctx context.Context) (*OutputProperty, error)
	BuilderDoLoad(ctx context.Context) (*OutputProperty, error)
	BuilderDoBuild(ctx context.Context) (*OutputProperty, error)
	BuilderDoSync(ctx context.Context) (*OutputProperty, error)
	BuilderDoDeploy(ctx context.Context) (*OutputProperty, error)

	// Runner interface

	RunnerDoLoad(ctx context.Context) (*OutputProperty, error)
	RunnerDoInit(ctx context.Context) (*OutputProperty, error)
	RunnerDoStart(ctx context.Context) (*OutputProperty, error)
	RunnerDoTest(ctx context.Context) (*OutputProperty, error)
	RunnerDoStop(ctx context.Context) (*OutputProperty, error)
	RunnerDoDestroy(ctx context.Context) (*OutputProperty, error)

	DoSetCallback(seed func(ctx context.Context, action Action) error)
}

/*
Manager is responsible is a wrapper around a service instance:
- keeps around request Dependencies
- keeps around ConfigurationManager information
*/
type Manager struct {
	service *resources.Service
	module  *resources.Module

	world *World

	Runner *Runner

	Builder *Builder

	initOnly bool

	load *runtimev0.LoadResponse
	init *runtimev0.InitResponse

	dependencyEndpoints []*basev0.Endpoint
	networkMappings     []*basev0.NetworkMapping
}

func (manager *Manager) BuilderDoInit(ctx context.Context) (*OutputProperty, error) {
	return manager.Builder.Init(ctx)
}

func (manager *Manager) BuilderDoLoad(ctx context.Context) (*OutputProperty, error) {
	return manager.Builder.Load(ctx)
}

func (manager *Manager) BuilderDoBuild(ctx context.Context) (*OutputProperty, error) {
	return manager.Builder.Build(ctx)
}

func (manager *Manager) BuilderDoSync(ctx context.Context) (*OutputProperty, error) {
	return manager.Builder.Sync(ctx)
}

func (manager *Manager) BuilderDoDeploy(ctx context.Context) (*OutputProperty, error) {
	return manager.Builder.Deploy(ctx)
}

func (manager *Manager) RunnerDoLoad(ctx context.Context) (*OutputProperty, error) {
	return manager.Runner.Load(ctx)
}

func (manager *Manager) RunnerDoInit(ctx context.Context) (*OutputProperty, error) {
	return manager.Runner.Init(ctx)
}

func (manager *Manager) RunnerDoStart(ctx context.Context) (*OutputProperty, error) {
	return manager.Runner.Start(ctx)
}

func (manager *Manager) RunnerDoTest(ctx context.Context) (*OutputProperty, error) {
	return manager.Runner.Test(ctx)
}

func (manager *Manager) RunnerDoStop(ctx context.Context) (*OutputProperty, error) {
	return manager.Runner.Stop(ctx)
}

func (manager *Manager) RunnerDoDestroy(ctx context.Context) (*OutputProperty, error) {
	return manager.Runner.Destroy(ctx)
}

func (manager *Manager) DoSetCallback(callback func(ctx context.Context, action Action) error) {
	manager.SetCallback(callback)
}

func (manager *Manager) Unique() string {
	return resources.WithUnique(manager.service).Unique()
}

func New(ctx context.Context, module *resources.Module, service *resources.Service, world *World) (*Manager, error) {
	w := wool.Get(ctx).In("hub.New", wool.ThisField(resources.WithUnique(service)))

	manager := &Manager{service: service, module: module, world: world}
	err := manager.Load(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}
	return manager, nil
}

func (manager *Manager) Load(ctx context.Context) error {
	w := wool.Get(ctx).In("hub.New", wool.ThisField(manager))

	instance, err := services.Load(ctx, manager.world.Workspace, manager.module, manager.service)
	if err != nil {
		return w.Wrapf(err, "cannot load service instance")
	}

	w.Debug("load agent", wool.Field("agent-pid", instance.ProcessInfo.AgentPID))

	switch manager.world.Mode {
	case RunMode, TestMode:
		w.Debug("load runtime")
		err = instance.LoadRuntime(ctx, true)
		if err != nil {
			return w.Wrapf(err, "cannot load service instance")
		}
		manager.Runner, err = NewRunner(ctx, instance, manager.world)
		if err != nil {
			return w.Wrapf(err, "cannot create runner")
		}
		err = manager.Runner.Follow(ctx)
		if err != nil {
			return w.Wrapf(err, "cannot follow service instance")
		}
		return nil
	case BuildMode, SyncMode, DeployMode:
		err = instance.LoadBuilder(ctx)
		if err != nil {
			return w.Wrapf(err, "cannot load service builder instance")
		}
		manager.Builder, err = NewBuilder(ctx, instance, manager.world)
		if err != nil {
			return w.Wrapf(err, "cannot create builder")
		}
		return nil
	}
	return w.NewError("unknown mode %s", manager.world.Mode)
}

func (manager *Manager) SetCallback(f Callback) {
	if manager.Runner == nil {
		return
	}
	manager.Runner.callback = f
}

type Hub struct {
	managers []IManager
}

func (hub *Hub) NewManager(unique string) (IManager, error) {
	for _, manager := range hub.managers {
		if manager.Unique() == unique {
			return manager, nil
		}
	}
	return nil, fmt.Errorf("no manager found for %s", unique)
}

type NoOpManager struct {
	service *resources.Service
}

func (n NoOpManager) RunnerDoTest(ctx context.Context) (*OutputProperty, error) {
	return nil, nil
}

func (n NoOpManager) RunnerDoStop(ctx context.Context) (*OutputProperty, error) {
	return nil, nil
}

func (n NoOpManager) RunnerDoDestroy(ctx context.Context) (*OutputProperty, error) {
	return nil, nil
}

func (n NoOpManager) Unique() string {
	return resources.WithUnique(n.service).Unique()
}

func (n NoOpManager) BuilderDoInit(ctx context.Context) (*OutputProperty, error) {
	return &OutputProperty{OnInit: true}, nil
}

func (n NoOpManager) BuilderDoLoad(ctx context.Context) (*OutputProperty, error) {
	return &OutputProperty{OnInit: true}, nil
}

func (n NoOpManager) BuilderDoBuild(ctx context.Context) (*OutputProperty, error) {
	return &OutputProperty{OnInit: true}, nil
}

func (n NoOpManager) BuilderDoSync(ctx context.Context) (*OutputProperty, error) {
	return &OutputProperty{OnInit: true}, nil
}

func (n NoOpManager) BuilderDoDeploy(ctx context.Context) (*OutputProperty, error) {
	return &OutputProperty{OnInit: true}, nil
}

func (n NoOpManager) RunnerDoLoad(ctx context.Context) (*OutputProperty, error) {
	return &OutputProperty{OnInit: true}, nil
}

func (n NoOpManager) RunnerDoInit(ctx context.Context) (*OutputProperty, error) {
	return &OutputProperty{OnInit: true}, nil
}

func (n NoOpManager) RunnerDoStart(ctx context.Context) (*OutputProperty, error) {
	return &OutputProperty{OnInit: true}, nil
}

func (n NoOpManager) DoSetCallback(seed func(ctx context.Context, action Action) error) {
	return
}

func (n NoOpManager) Stop(ctx context.Context) error {
	return nil
}
