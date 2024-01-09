package factory

import (
	"context"

	"github.com/codefly-dev/cli/pkg/services"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	factoryv0 "github.com/codefly-dev/core/generated/go/services/factory/v0"
	"github.com/codefly-dev/core/wool"
)

type ActionType int

const (
	Noop ActionType = iota
	Load
	Init
	Sync // Sync the service
	Build
)

// Action represents an action to be taken on a service by the runner
type Action struct {
	Type   ActionType
	Unique string
	Only   bool
}

/*
Manager is responsible for the life-cycle of a service
- Runner is a wrapping around a service instance
- Actions channel to affect life-cycle of the service (start, stop, restart)
*/
type Manager struct {
	service  *configurations.Service
	initOnly bool
	instance *services.Instance
	actions  chan Action

	loaded *factoryv0.LoadResponse
	init   *factoryv0.InitResponse
	build  *factoryv0.BuildResponse

	dependencyEndpoints []*basev0.Endpoint
}

func (manager *Manager) Unique() string {
	return manager.service.Unique()
}

func New(ctx context.Context, service *configurations.Service) (*Manager, error) {
	// Use buffer of size 1: more difficult but makes sure the logic is sound
	manager := &Manager{service: service, actions: make(chan Action, 1)}
	return manager, nil
}

func (manager *Manager) Load(ctx context.Context) error {
	w := wool.Get(ctx).In("factory.manager::Load", wool.ThisField(manager.service))
	instance, err := services.Load(ctx, manager.service)
	if err != nil {
		return w.Wrapf(err, "cannot load service instance")
	}

	if instance.Factory == nil {
		return w.Wrapf(err, "no runtime is implemented for service")
	}

	loaded, err := instance.Factory.Load(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot load service instance")
	}
	Register(ctx, instance)

	manager.instance = instance
	manager.loaded = loaded
	return nil
}

// Init the service
func (manager *Manager) Init(ctx context.Context) error {
	w := wool.Get(ctx).In("factory.manager::Init", wool.ThisField(manager))
	req := &factoryv0.InitRequest{DependenciesEndpoints: manager.dependencyEndpoints}
	init, err := manager.instance.Factory.Init(ctx, req)
	if err != nil {
		return w.NewError("cannot Init service instance")
	}
	manager.init = init
	return nil
}

func (manager *Manager) Sync(ctx context.Context) error {
	w := wool.Get(ctx).In("service.Run", wool.ThisField(manager))
	req := &factoryv0.SyncRequest{}
	sync, err := manager.instance.Factory.Sync(ctx, req)
	if err != nil {
		return w.Wrapf(err, "cannot sync service instance")
	}
	w.Debug("sync", wool.ResponseField(sync).Trace())
	return nil
}

func (manager *Manager) WithEndpointDependencies(endpoints []*basev0.Endpoint) *Manager {
	manager.dependencyEndpoints = endpoints
	return manager

}

func (manager *Manager) Build(ctx context.Context) error {
	w := wool.Get(ctx).In("service.Build", wool.ThisField(manager))
	req := &factoryv0.BuildRequest{}
	build, err := manager.instance.Factory.Build(ctx, req)
	if err != nil {
		return w.Wrapf(err, "cannot build service instance")
	}
	manager.build = build
	return nil
}
