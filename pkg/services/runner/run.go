package runner

import (
	"context"
	"fmt"
	"time"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	runtimev1 "github.com/codefly-dev/core/generated/go/services/runtime/v1"
	"github.com/codefly-dev/core/runners"
	"github.com/codefly-dev/core/wool"
)

type RunManager struct {
	runner *Runner
}

func New(ctx context.Context, service *configurations.Service) (*RunManager, error) {
	// Create a runner
	runner, err := NewRunner(ctx, service)
	if err != nil {
		return nil, err
	}
	manager := &RunManager{runner: runner}
	return manager, nil
}

// Start handles the life-cycle of the service
func (manager *RunManager) Start(ctx context.Context) error {
	w := wool.Get(ctx).In("service.Start", wool.ThisField(manager.runner.instance.Service))
	actions := make(chan runners.Action, 10)
	actions <- runners.Action{Type: runners.Init}
	for {
		select {
		case action := <-actions:
			switch action.Type {
			case runners.Init:
				err := manager.runner.Init(ctx)
				if err != nil {
					w.Info("cannot initialize service", wool.ErrField(err))
				} else {
					actions <- runners.Action{Type: runners.Start}
				}
			case runners.Start:
				err := manager.runner.Run(ctx, actions)
				if err != nil {
					return w.Wrapf(err, "can't run")
				}
			case runners.Restart:
				w.Info("restarting")
				err := manager.Stop()
				if err != nil {
					return w.Wrapf(err, "can't stop")
				}
				// Create new runner
				runner, err := NewRunner(ctx, manager.runner.instance.Service)
				if err != nil {
					return w.Wrapf(err, "can't create new runner")
				}
				manager.runner = runner
				actions <- runners.Action{Type: runners.Init}
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
	w := wool.Get(ctx).In("service.Stop", wool.ThisField(manager.runner.instance.Service))
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx = w.Inject(ctx)
	err := manager.runner.Stop(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot stop service")
	}
	return nil
}

type Runner struct {
	instance *services.ServiceInstance
}

func NewRunner(ctx context.Context, service *configurations.Service) (*Runner, error) {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(service))
	instance, err := services.Load(ctx, service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	w.Debug("loaded agent", wool.Field("agent-pid", instance.ProcessInfo.AgentPID))

	if instance.Runtime == nil {
		return nil, w.Wrapf(err, "no runtime is implemented for service")
	}

	loaded, err := instance.Runtime.Load(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}
	w.Debug("loaded runtime", wool.ResponseField(loaded).Trace())
	Register(ctx, instance)
	return &Runner{instance: instance}, nil
}

// Init the service
func (runner *Runner) Init(ctx context.Context) error {
	w := wool.Get(ctx).In("service.Init", wool.ThisField(runner.instance.Service))
	conf, err := runner.instance.Runtime.Init(ctx, &runtimev1.InitRequest{})
	if err != nil {
		return w.NewError("cannot Init service instance: %v", conf)
	}
	w.Debug("Init", wool.ResponseField(conf).Trace())
	return nil
}

// Run the Start of a service and setup monitoring
// Events can trigger actions
func (runner *Runner) Run(ctx context.Context, actions chan runners.Action) error {
	w := wool.Get(ctx).In("service.Run", wool.ThisField(runner.instance.Service))

	start, err := runner.instance.Runtime.Start(ctx, &runtimev1.StartRequest{})
	if err != nil {
		return w.Wrapf(err, "cannot start service instance")
	}
	if start.Status.State != runtimev1.StartStatus_STARTED {
		return w.Wrapf(fmt.Errorf(start.Status.Message), "cannot start service instance")
	}

	w.Debug("start", wool.ResponseField(start).Trace())

	follow, err := runner.Follow(ctx, actions)
	if err != nil {
		return w.Wrapf(err, "cannot follow service")
	}

	observe, err := runner.Observe(ctx, start.Trackers)
	if err != nil {
		return w.Wrapf(err, "cannot observe service")
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-follow:
				err := runner.Handle(ctx, event)
				if err != nil {
					w.Debug("cannot handle follow event", wool.ErrField(err))
				}
			case event := <-observe:
				err := runner.Handle(ctx, event)
				if err != nil {
					w.Debug("cannot handle observe event", wool.ErrField(err))
				}
			}
		}
	}()
	return nil
}

// Follow calls the agent for information and generate a channel of events for the service:
// - Handle restart
func (runner *Runner) Follow(ctx context.Context, actions chan runners.Action) (chan runners.Event, error) {
	w := wool.Get(ctx).In("service.Follow", wool.ThisField(runner.instance.Service))
	events := make(chan runners.Event)

	go func() {
		for {
			info, err := runner.instance.Runtime.Information(ctx, &runtimev1.InformationRequest{})
			w.Trace("info", wool.ResponseField(info))
			if err != nil {
				events <- runners.Event{Err: err}
				return
			}
			if info.DesiredState == services.DesiredRestart {
				w.Info("want a restart")
				actions <- runners.Action{Type: runners.Restart}
			}
			time.Sleep(1000 * time.Millisecond)
		}
	}()
	return events, nil
}

func (runner *Runner) Observe(ctx context.Context, trackers []*runtimev1.Tracker) (chan runners.Event, error) {
	w := wool.Get(ctx).In("service.Observe", wool.ThisField(runner.instance.Service))

	events, err := runners.Track(ctx, trackers)
	if err != nil {
		return nil, w.Wrapf(err, "cannot create tracker")
	}
	return events, nil

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
