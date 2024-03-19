package manager

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/services/services"

	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
	"github.com/codefly-dev/core/wool"
)

type Mode string

const (
	RunMode    Mode = "run"
	SyncMode   Mode = "sync"
	BuildMode  Mode = "build"
	DeployMode Mode = "deploy"
)

type IManager interface {
	Unique() string

	// Builder
	BuilderDoInit(ctx context.Context) (*OutputProperty, error)
	BuilderDoLoad(ctx context.Context) (*OutputProperty, error)
	BuilderDoBuild(ctx context.Context) (*OutputProperty, error)
	BuilderDoSync(ctx context.Context) (*OutputProperty, error)
	BuilderDoDeploy(ctx context.Context) (*OutputProperty, error)

	// Runner
	RunnerDoLoad(ctx context.Context) (*OutputProperty, error)
	RunnerDoInit(ctx context.Context) (*OutputProperty, error)
	RunnerDoStart(ctx context.Context) (*OutputProperty, error)

	DoSetCallback(seed func(ctx context.Context, action Action) error)

	Stop(ctx context.Context) error
}

/*
Manager is responsible is a wrapper around a service instance:
- keeps around request Dependencies
- keeps around Provider information
*/
type Manager struct {
	service *configurations.Service

	world *World

	Runner *Runner

	Builder *Builder

	initOnly bool

	load *runtimev0.LoadResponse
	init *runtimev0.InitResponse

	dependencyEndpoints []*basev0.Endpoint
	networkMappings     []*basev0.NetworkMapping
	providerInfos       []*basev0.ProviderInformation
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

func (manager *Manager) DoSetCallback(callback func(ctx context.Context, action Action) error) {
	manager.SetCallback(callback)
}

func (manager *Manager) Unique() string {
	return manager.service.Unique()
}

func New(ctx context.Context, service *configurations.Service, world *World) (*Manager, error) {
	w := wool.Get(ctx).In("hub.New", wool.ThisField(service))

	manager := &Manager{service: service, world: world}
	err := manager.Load(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}
	return manager, nil
}

func (manager *Manager) Load(ctx context.Context) error {
	w := wool.Get(ctx).In("hub.New", wool.ThisField(manager.service))

	instance, err := services.Load(ctx, manager.service)
	if err != nil {
		return w.Wrapf(err, "cannot load service instance")
	}

	w.Debug("load agent", wool.Field("agent-pid", instance.ProcessInfo.AgentPID))

	switch manager.world.Mode {
	case RunMode:
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

func (manager *Manager) Stop(ctx context.Context) error {
	if manager.world.Mode == RunMode {
		return manager.Runner.Stop(ctx)
	}
	return nil
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

func (hub *Hub) Manager(unique string) (IManager, error) {
	for _, manager := range hub.managers {
		if manager.Unique() == unique {
			return manager, nil
		}
	}
	return nil, fmt.Errorf("no manager found for %s", unique)
}

type NoOpManager struct {
	service *configurations.Service
}

func (n NoOpManager) Unique() string {
	return n.service.Unique()
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
