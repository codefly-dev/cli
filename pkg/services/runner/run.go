package runner

import (
	"context"
	"fmt"
	"time"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	basev1 "github.com/codefly-dev/core/generated/go/base/v1"
	runtimev1 "github.com/codefly-dev/core/generated/go/services/runtime/v1"
	"github.com/codefly-dev/core/runners"
	"github.com/codefly-dev/core/wool"
)

/*
RunManager is responsible for the life-cycle of a service
- Runner is a wrapping around a service instance
- Actions channel to affect life-cycle of the service (start, stop, restart)
*/
type RunManager struct {
	service  *configurations.Service
	runner   *Runner
	initOnly bool
	actions  chan runners.Action

	loaded              *runtimev1.LoadResponse
	init                *runtimev1.InitResponse
	dependencyEndpoints []*basev1.Endpoint
	networkMappings     []*runtimev1.NetworkMapping
}

func (manager *RunManager) Unique() string {
	return manager.runner.instance.Service.Unique()
}

func New(ctx context.Context, service *configurations.Service) (*RunManager, error) {
	// Use buffer of size 1: more difficult but makes sure the logic is sound
	manager := &RunManager{service: service, actions: make(chan runners.Action, 1)}
	// Create a runner
	err := manager.Load(ctx)
	if err != nil {
		return nil, err
	}
	return manager, nil
}

// Start handles the life-cycle of the service
func (manager *RunManager) Start(ctx context.Context) error {
	w := wool.Get(ctx).In("service.Start", wool.ThisField(manager))
	manager.actions <- runners.Action{Type: runners.Init}
	for {
		select {
		case action := <-manager.actions:
			switch action.Type {
			case runners.Noop:
			case runners.Init:
				err := manager.Init(ctx)
				if err != nil {
					w.Debug("cannot initialize service")
					manager.actions <- runners.Action{Type: runners.Noop}
				} else if manager.initOnly {
					manager.actions <- runners.Action{Type: runners.Noop}
				} else {
					manager.actions <- runners.Action{Type: runners.Start}
				}
			case runners.Start:
				err := manager.Run(ctx)
				if err != nil {
					w.Debug("cannot start service")
				}
				manager.actions <- runners.Action{Type: runners.Noop}
			case runners.Restart:
				w.Info("restarting")
				err := manager.Stop()
				if err != nil {
					return w.Wrapf(err, "can't stop")
				}
				// Create new runner
				err = manager.Load(ctx)
				if err != nil {
					return w.Wrapf(err, "can't create new runner")
				}
				manager.actions <- runners.Action{Type: runners.Init}
			default:
				return w.NewError("unknown action type")
			}
		case <-ctx.Done():
			return manager.Stop()
		}
	}
}

func (manager *RunManager) Stop() error {
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

func (manager *RunManager) Load(ctx context.Context) error {
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
func (manager *RunManager) Init(ctx context.Context) error {
	w := wool.Get(ctx).In("service.Init", wool.ThisField(manager))
	req := &runtimev1.InitRequest{DependenciesEndpoints: manager.dependencyEndpoints}
	init, err := manager.runner.instance.Runtime.Init(ctx, req)
	if err != nil {
		return w.NewError("cannot Init service instance")
	}
	manager.init = init
	SetNetworkMappings(manager.Unique(), manager.init.NetworkMappings)
	return nil
}

// Run the Start of a service and setup monitoring
// Events can trigger actions
func (manager *RunManager) Run(ctx context.Context) error {
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

/*
Runner is a wrapper around a service instance:
- collects events from the agent API
- collects events from the service instance observability
*/
type Runner struct {
	instance *services.ServiceInstance
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
func (manager *RunManager) Follow(ctx context.Context) error {
	w := wool.Get(ctx).In("service.Follow", wool.ThisField(manager.runner.instance.Service))

	go func() {
		for {
			info, err := manager.runner.instance.Runtime.Information(ctx, &runtimev1.InformationRequest{})
			w.Trace("info", wool.ResponseField(info))
			if err != nil {
				manager.runner.events <- runners.Event{Err: err}
				return
			}
			if info.DesiredState == services.DesiredRestart {
				w.Info("want a restart")
				manager.actions <- runners.Action{Type: runners.Restart}
			}
			time.Sleep(1000 * time.Millisecond)
		}
	}()
	return nil
}

func (manager *RunManager) WithEndpointDependencies(endpoints []*basev1.Endpoint) *RunManager {
	manager.dependencyEndpoints = endpoints
	return manager

}

func (manager *RunManager) WithNetworkMappings(mappings []*runtimev1.NetworkMapping) *RunManager {
	manager.networkMappings = mappings
	return manager
}

func (manager *RunManager) InitOnly(only bool) {
	manager.initOnly = only
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
