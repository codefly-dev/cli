package manager

import (
	"context"
	"fmt"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/codefly-dev/cli/pkg/services/network"
	"github.com/codefly-dev/cli/pkg/services/services"

	"github.com/codefly-dev/core/configurations"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
	"github.com/codefly-dev/core/wool"
)

/*
Runner is a wrapper around a runtime service instance to fit the outputProperty interface

- collects events from the agent API
- collects events from the service instance observability
*/
type Runner struct {
	instance *services.Instance

	// API
	endpoints []*basev0.Endpoint

	// View of the world
	world *World

	// Callback
	callback Callback

	// Requires
	requires []string

	// outputProperty hub
	isStarted bool
	restart   bool

	outputPropertyForLoad  *RunnerLoadManager
	outputPropertyForInit  *RunnerInitManager
	outputPropertyForStart *RunnerStartManager

	stopped chan struct{}
}

type Callback func(ctx context.Context, action Action) error

func NewRunner(ctx context.Context, instance *services.Instance, world *World) (*Runner, error) {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(instance))
	w.Debug("new")
	runner := &Runner{
		instance: instance,

		world: world,

		outputPropertyForLoad:  NewRunnerLoadManager(instance.Unique()),
		outputPropertyForInit:  NewRunnerInitManager(instance.Unique()),
		outputPropertyForStart: NewRunnerStartManager(instance.Unique()),

		stopped: make(chan struct{}),
	}
	return runner, nil
}

func (runner *Runner) Load(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("Runner.Load", wool.ThisField(runner.instance.Service))

	env, err := runner.world.Env.Proto()
	if err != nil {
		return nil, w.Wrapf(err, "cannot get environment")
	}
	resp, err := runner.instance.Runtime.Load(ctx, env)
	if err != nil {
		if ContextCancelled(err) {
			return nil, nil
		}
		w.Warn(fmt.Sprintf("cannot load runtime instance for <%s> %v", runner.instance.Unique(), err))
		err = runner.outputPropertyForLoad.Set(ctx, &RunnerLoadOutput{Err: err.Error()})
		if err != nil {
			return nil, w.Wrapf(err, "cannot set outputProperty for load")
		}
		return runner.outputPropertyForLoad.Process(ctx)
	}
	if resp.Status != nil && resp.Status.State != runtimev0.LoadStatus_READY {
		w.Warn(fmt.Sprintf("load returned error: %v", resp.Status.Message))
		err = runner.outputPropertyForLoad.Set(ctx, &RunnerLoadOutput{Err: resp.Status.Message})
		if err != nil {
			return nil, w.Wrapf(err, "cannot set outputProperty for load")
		}
		return runner.outputPropertyForLoad.Process(ctx)
	}

	w.Debug("loaded",
		wool.Field("endpoints", configurations.MakeEndpointSummary(resp.Endpoints)))

	runner.endpoints = resp.Endpoints

	err = runner.world.SharedState.RecordEndpoints(ctx, runner.instance.Service, resp.Endpoints)
	if err != nil {
		return nil, w.Wrapf(err, "cannot record endpoints")
	}

	err = runner.outputPropertyForLoad.Set(ctx, &RunnerLoadOutput{Endpoints: resp.Endpoints})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for load")
	}

	return runner.outputPropertyForLoad.Process(ctx)
}

func generatePortNetworkMappings(ctx context.Context, endpoints []*basev0.Endpoint) ([]*basev0.NetworkMapping, error) {
	w := wool.Get(ctx).In("service.NewRunner")
	w.Debug("endpoints", wool.NullableField("got", configurations.MakeEndpointSummary(endpoints)))

	pm, err := network.NewServicePortManager(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot create default endpoint")
	}
	for _, endpoint := range endpoints {
		w.Debug("exposing", wool.Field("destination", configurations.EndpointDestination(endpoint)))
		err = pm.Expose(endpoint)
		if err != nil {
			return nil, w.Wrapf(err, "cannot add grpc endpoint to network manager")
		}
	}
	err = pm.Reserve(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot reserve ports")
	}
	networkMappings, err := pm.NetworkMapping(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot create network mapping")
	}
	w.Debug("network mappings", wool.Field("mappings", configurations.MakeNetworkMappingSummary(networkMappings)))
	return networkMappings, nil
}

func ContextCancelled(err error) bool {
	if grpcErr, ok := status.FromError(err); ok {
		// Now grpcErr is the unwrapped gRPC error
		// You can get the error code and message like this
		code := grpcErr.Code()
		// Check if the error is a context cancelled error
		if code == codes.Canceled {
			return true
		}
	}
	return false
}

func (runner *Runner) Init(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("Runner.Init", wool.ThisField(runner.instance.Service))
	w.Debug("init")

	dependenciesEndpoints, err := runner.world.SharedState.GetDependenciesEndpoints(ctx, runner.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot run init")
	}

	infos, err := runner.world.SharedState.GetProviderInfos(ctx, runner.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get provider info")
	}

	// Get all the shared provider info from the dependents

	networkMappings, err := generatePortNetworkMappings(ctx, runner.endpoints)
	if err != nil {
		return nil, w.Wrapf(err, "cannot generate network mappings")
	}

	resp, err := runner.instance.Runtime.Init(ctx, &runtimev0.InitRequest{
		ProposedNetworkMappings: networkMappings,
		ProviderInfos:           infos,
		DependenciesEndpoints:   dependenciesEndpoints,
	})
	if err != nil {
		if ContextCancelled(err) {
			return nil, nil
		}
		return nil, w.Wrapf(err, "cannot call init")
	}

	if resp.Status != nil && resp.Status.State != runtimev0.InitStatus_READY {
		w.Focus("init failed: waiting")
		err = runner.outputPropertyForInit.Set(ctx, &RunnerInitOutput{failing: true})
		return runner.outputPropertyForInit.Process(ctx)

	}
	err = runner.world.SharedState.RecordNetworkMappings(ctx, runner.instance.Service, resp.NetworkMappings)
	if err != nil {
		return nil, w.Wrapf(err, "cannot record network mappings")
	}

	w.Debug("init",
		wool.Field("provider info", configurations.MakeProviderInformationSummary(resp.ServiceProviderInfos)))

	err = runner.outputPropertyForInit.Set(ctx, &RunnerInitOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for init")
	}

	outputProperty, err := runner.outputPropertyForInit.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for init")
	}

	err = runner.world.SharedState.RecordSharedProviderInfos(ctx, runner.instance.Service, resp.ServiceProviderInfos)
	if err != nil {
		return nil, w.Wrapf(err, "cannot record shared provider infos")
	}

	return outputProperty, nil
}

func (runner *Runner) Start(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(runner.instance.Service))
	w.Debug("start")

	err := runner.StopIfNeeded(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot stop service instance")
	}

	// Build the request
	networkMappings, err := runner.world.SharedState.GetNetworkMappings(ctx, runner.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	w.Debug("starting", wool.Field("networkMappings", configurations.MakeNetworkMappingSummary(networkMappings)))

	resp, err := runner.instance.Runtime.Start(ctx, &runtimev0.StartRequest{OtherNetworkMappings: networkMappings})
	if err != nil {
		if ContextCancelled(err) {
			return nil, nil
		}
		return nil, w.Wrapf(err, "cannot start service instance")
	}

	if resp.Status != nil && resp.Status.State != runtimev0.StartStatus_STARTED {
		return nil, w.NewError("service instance is not started")
	}

	err = runner.outputPropertyForStart.Set(ctx, &RunnerStartOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for start")
	}

	outputProperty, err := runner.outputPropertyForLoad.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for start")
	}

	w.Debug("outputProperty", wool.Field("outputProperty", outputProperty))
	runner.isStarted = true
	return outputProperty, nil
}

func (runner *Runner) StopIfNeeded(ctx context.Context) error {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(runner.instance.Service))
	w.Debug("stopIfNeeded", wool.Field("isStarted", runner.isStarted), wool.Field("isHotReloading", runner.instance.Runtime.IsHotReloading))
	if !runner.isStarted {
		return nil
	}
	if runner.instance.Runtime.IsHotReloading {
		return nil
	}
	w.Debug("stopping")
	// Build the request
	runner.isStarted = false
	runner.stopped <- struct{}{}

	_, err := runner.instance.Runtime.Stop(ctx, &runtimev0.StopRequest{})
	if err != nil {
		return w.Wrapf(err, "cannot stop service instance")
	}
	return nil

}

func (runner *Runner) Stop(ctx context.Context) error {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(runner.instance.Service))
	w.Debug("stopping")
	// Build the request
	runner.isStarted = false
	go func() {
		runner.stopped <- struct{}{}
	}()

	_, err := runner.instance.Runtime.Stop(ctx, &runtimev0.StopRequest{})
	if err != nil {
		return w.Wrapf(err, "cannot stop service instance")
	}
	return nil
}

// Follow calls the agent for information and generate a channel of events for the service:
// - Handle restart
func (runner *Runner) Follow(ctx context.Context) error {
	w := wool.Get(ctx).In("service.Follow", wool.ThisField(runner.instance.Service))
	go func() {
		for {
			select {
			case <-runner.stopped:
				if !runner.restart {
					return
				}
			default:
				info, err := runner.instance.Runtime.Information(ctx, &runtimev0.InformationRequest{})
				if err != nil {
					w.Debug("cannot get information", wool.ErrField(err))
					return
				}
				if info.DesiredState != nil && info.DesiredState.Stage != runtimev0.DesiredState_NOOP {
					w.Debug("received a request to change sharedState", wool.Field("sharedState", info.DesiredState.Stage))
					action := Action{Service: runner.Unique()}
					switch info.DesiredState.Stage {
					case runtimev0.DesiredState_LOAD:
						action.Type = RuntimeLoad
					case runtimev0.DesiredState_INIT:
						action.Type = RuntimeInit
					case runtimev0.DesiredState_START:
						action.Type = RuntimeStart
					}
					runner.restart = true
					w.Debug("sending action", wool.Field("action", action.Type))
					err = runner.callback(ctx, action)
					if err != nil {
						w.Error("cannot seed", wool.ErrField(err))
						return
					}
				}
				time.Sleep(1000 * time.Millisecond)
			}
		}
	}()
	return nil
}

func (runner *Runner) Unique() string {
	return runner.instance.Service.Unique()
}
