package manager

import (
	"context"

	"github.com/codefly-dev/cli/pkg/services"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
	"github.com/codefly-dev/core/wool"
)

type Mode string

const (
	RunMode   Mode = "run"
	BuildMode Mode = "build"
)

/*
Manager is responsible is a wrapper around a service instance:
- keeps around request dependencies
- keeps around provider information
*/
type Manager struct {
	service *configurations.Service

	world *World

	actions *ActionManager

	Runner *Runner

	initOnly bool

	load *runtimev0.LoadResponse
	init *runtimev0.InitResponse

	dependencyEndpoints []*basev0.Endpoint
	networkMappings     []*basev0.NetworkMapping
	providerInfos       []*basev0.ProviderInformation
}

func (manager *Manager) Unique() string {
	return manager.service.Unique()
}

func New(ctx context.Context, service *configurations.Service, world *World, actions *ActionManager) (*Manager, error) {
	w := wool.Get(ctx).In("managers.New", wool.ThisField(service))

	manager := &Manager{service: service, world: world, actions: actions}
	err := manager.Load(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}
	return manager, nil
}

func (manager *Manager) Load(ctx context.Context) error {
	w := wool.Get(ctx).In("managers.New", wool.ThisField(manager.service))

	instance, err := services.Load(ctx, manager.service)
	if err != nil {
		return w.Wrapf(err, "cannot load service instance")
	}

	w.Debug("load agent", wool.Field("agent-pid", instance.ProcessInfo.AgentPID))

	switch manager.world.mode {
	case RunMode:
		w.Focus("load runtime")
		err = instance.LoadRuntime(ctx)
		if err != nil {
			return w.Wrapf(err, "cannot load service instance")
		}
		manager.Runner, err = NewRunner(ctx, instance, manager.world, manager.actions)
		if err != nil {
			return w.Wrapf(err, "cannot create runner")
		}
		err = manager.Runner.Follow(ctx)
		if err != nil {
			return w.Wrapf(err, "cannot follow service instance")
		}
	case BuildMode:
		err = instance.LoadBuilder(ctx)
		if err != nil {
			return w.Wrapf(err, "cannot load service builder instance")
		}
	}
	return nil
}

func (manager *Manager) Stop(ctx context.Context) error {
	if manager.world.mode == RunMode {
		return manager.Runner.Stop(ctx)
	}
	return nil
}
