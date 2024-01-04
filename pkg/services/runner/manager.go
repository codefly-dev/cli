package runner

import (
	"context"
	"fmt"
	"time"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/services"
	agentservices "github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	basev1 "github.com/codefly-dev/core/generated/go/base/v1"
	runtimev1 "github.com/codefly-dev/core/generated/go/services/runtime/v1"
	"github.com/codefly-dev/core/runners"
	"github.com/codefly-dev/core/wool"
)

type ActionType int

const (
	Noop ActionType = iota
	Load
	Init
	Start   // Start the service
	Stop    // Stop the service
	Restart // Restart the service
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
	runner   *Runner
	initOnly bool
	actions  chan Action

	loaded              *runtimev1.LoadResponse
	init                *runtimev1.InitResponse
	dependencyEndpoints []*basev1.Endpoint
	networkMappings     []*runtimev1.NetworkMapping
}

func (manager *Manager) Unique() string {
	return manager.service.Unique()
}

func New(ctx context.Context, service *configurations.Service, actions chan Action) (*Manager, error) {
	manager := &Manager{service: service, actions: actions}
	return manager, nil
}

func (manager *Manager) Load(ctx context.Context) error {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(manager.service))
	instance, err := services.Load(ctx, manager.service)
	if err != nil {
		return w.Wrapf(err, "cannot load service instance")
	}

	w.Debug("loaded agent", wool.Field("agent-pid", instance.ProcessInfo.AgentPID))

	if instance.Runtime == nil {
		return w.Wrapf(err, "no runtime is implemented for service")
	}

	loaded, err := instance.Runtime.Load(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot load service instance")
	}
	Register(ctx, instance)

	manager.runner = &Runner{instance: instance, events: make(chan runners.Event)}
	err = manager.Follow(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot follow service instance")
	}

	manager.loaded = loaded
	return nil
}

// Init the service
func (manager *Manager) Init(ctx context.Context) error {
	w := wool.Get(ctx).In("service.Init", wool.ThisField(manager))
	req := &runtimev1.InitRequest{DependenciesEndpoints: manager.dependencyEndpoints}
	init, err := manager.runner.instance.Runtime.Init(ctx, req)
	if err != nil {
		return w.NewError("cannot Init service instance")
	}
	manager.init = init
	return nil
}

// Run the Start of a service and setup monitoring
// Events can trigger actions
func (manager *Manager) Run(ctx context.Context) error {
	w := wool.Get(ctx).In("service.Run", wool.ThisField(manager))
	req := &runtimev1.StartRequest{NetworkMappings: manager.networkMappings}
	start, err := manager.runner.instance.Runtime.Start(ctx, req)
	if err != nil {
		return w.Wrapf(err, "cannot start service instance")
	}
	if start.Status.State != runtimev1.StartStatus_STARTED {
		return w.Wrapf(fmt.Errorf(start.Status.Message), "cannot start service instance")
	}
	w.Debug("start", wool.ResponseField(start).Trace())
	return nil
}

func (manager *Manager) Stop() error {
	// New context for stopping!
	ctx := context.Background()
	w := wool.Get(ctx).In("service.Stop", wool.ThisField(manager))
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx = w.Inject(ctx)
	err := manager.runner.Stop(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot stop service")
	}
	return nil
}

/*
Runner is a wrapper around a service instance:
- collects events from the agent API
- collects events from the service instance observability
*/
type Runner struct {
	instance *services.Instance
	events   chan runners.Event
}

func (runner *Runner) Listen(ctx context.Context) error {
	w := wool.Get(ctx).In("service.Run", wool.ThisField(runner.instance.Service))
	for {
		select {
		case <-ctx.Done():
			return nil
		case event := <-runner.events:
			err := runner.Handle(ctx, event)
			if err != nil {
				w.Debug("cannot handle follow event", wool.ErrField(err))
			}
		}
	}
}

// Follow calls the agent for information and generate a channel of events for the service:
// - Handle restart
func (manager *Manager) Follow(ctx context.Context) error {
	w := wool.Get(ctx).In("service.Follow", wool.ThisField(manager.runner.instance.Service))

	go func() {
		for {
			info, err := manager.runner.instance.Runtime.Information(ctx, &runtimev1.InformationRequest{})
			w.Trace("info", wool.ResponseField(info))
			if err != nil {
				manager.runner.events <- runners.Event{Err: err}
				return
			}
			if info.DesiredState == agentservices.DesiredRestart {
				w.Info("want a restart")
				manager.actions <- Action{Type: Restart, Unique: manager.Unique()}
			}
			time.Sleep(1000 * time.Millisecond)
		}
	}()
	return nil
}

func (manager *Manager) WithEndpointDependencies(endpoints []*basev1.Endpoint) *Manager {
	manager.dependencyEndpoints = endpoints
	return manager
}

func (manager *Manager) WithNetworkMappings(mappings []*runtimev1.NetworkMapping) *Manager {
	manager.networkMappings = mappings
	return manager
}

func (manager *Manager) InitOnly(only bool) {
	manager.initOnly = only
}

func (manager *Manager) Sync(ctx context.Context) error {
	return nil
}

func (runner *Runner) Stop(ctx context.Context) error {
	cli.Header(2, "Stopping service %s", runner.instance.Service.Name)
	_, err := runner.instance.Runtime.Stop(ctx, &runtimev1.StopRequest{})
	if err != nil {
		return err
	}
	return nil
}

func (runner *Runner) Handle(ctx context.Context, event runners.Event) error {
	w := wool.Get(ctx).In("service.Handle", wool.ThisField(runner.instance.Service))
	if event.Err != nil {
		return w.Wrap(event.Err)
	}
	if event.CPU != nil {
		w.Trace("CPU", wool.Field("usage", event.CPU.Usage))
	}
	return nil
}
