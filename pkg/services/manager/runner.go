package manager

import (
	"context"
	"fmt"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/shared"

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
	endpoints       []*basev0.Endpoint
	networkMappings []*basev0.NetworkMapping

	// For "callbacks"
	playbook *Playbook

	// State
	sharedState *StateManager

	// Requires
	requires []string

	// outputProperty managers
	isStarted bool

	outputPropertyForLoad  *RunnerLoadManager
	outputPropertyForInit  *RunnerInitManager
	outputPropertyForStart *RunnerStartManager

	stopped chan struct{}
}

func NewRunner(ctx context.Context, instance *services.Instance, playbook *Playbook, sharedState *StateManager) (*Runner, error) {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(instance))
	w.Debug("new")
	runner := &Runner{
		instance: instance,

		playbook: playbook,

		sharedState: sharedState,

		outputPropertyForLoad:  NewRunnerLoadManager(instance.Unique()),
		outputPropertyForInit:  NewRunnerInitManager(instance.Unique()),
		outputPropertyForStart: NewRunnerStartManager(instance.Unique()),

		stopped: make(chan struct{}),
	}
	return runner, nil
}

func (runner *Runner) Load(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(runner.instance.Service))

	resp, err := runner.instance.Runtime.Load(ctx, shared.Must(runner.playbook.world.Env.Proto()))
	if err != nil {
		w.Warn(fmt.Sprintf("cannot load service instance %v", err))
		err = runner.outputPropertyForLoad.Set(ctx, &RunnerLoadOutput{Err: err.Error()})
		if err != nil {
			return nil, w.Wrapf(err, "cannot set outputProperty for load")
		}
		return runner.outputPropertyForLoad.Process(ctx)

	}
	if resp.Status.State != runtimev0.LoadStatus_READY {
		w.Warn(fmt.Sprintf("cannot load service instance %v", resp.Status.Message))
		err = runner.outputPropertyForLoad.Set(ctx, &RunnerLoadOutput{Err: resp.Status.Message})
		if err != nil {
			return nil, w.Wrapf(err, "cannot set outputProperty for load")
		}
		return runner.outputPropertyForLoad.Process(ctx)
	}

	w.Debug("loaded",
		wool.Field("endpoints", configurations.MakeEndpointSummary(resp.Endpoints)))

	runner.endpoints = resp.Endpoints

	networkMappings, err := generatePortNetworkMappings(ctx, resp.Endpoints)
	if err != nil {
		return nil, w.Wrapf(err, "cannot generate network mappings")
	}
	runner.networkMappings = networkMappings

	err = runner.sharedState.RecordNetworkMappings(ctx, runner.instance.Service, runner.networkMappings)
	if err != nil {
		return nil, w.Wrapf(err, "cannot record network mappings")
	}

	err = runner.sharedState.RecordEndpoints(ctx, runner.instance.Service, resp.Endpoints)
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

func (runner *Runner) Init(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(runner.instance.Service))
	w.Debug("init")

	dependenciesEndpoints, err := runner.sharedState.GetDependenciesEndpoints(ctx, runner.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	infos, err := runner.sharedState.GetProviderInfos(ctx, runner.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	// Get all the shared provider info from the dependents

	resp, err := runner.instance.Runtime.Init(ctx, &runtimev0.InitRequest{
		NetworkMappings:       runner.networkMappings,
		ProviderInfos:         infos,
		DependenciesEndpoints: dependenciesEndpoints,
	})
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	if resp.Status.State != runtimev0.InitStatus_READY {
		return nil, w.NewError("service instance is not ready")
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

	err = runner.sharedState.RecordSharedProviderInfos(ctx, runner.instance.Service, resp.ServiceProviderInfos)
	if err != nil {
		return nil, w.Wrapf(err, "cannot record shared provider infos")
	}

	w.Debug("outputProperty", wool.Field("outputProperty", outputProperty))
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
	networkMappings, err := runner.sharedState.GetNetworkMappings(ctx, runner.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	w.Debug("starting", wool.Field("networkMappings", configurations.MakeNetworkMappingSummary(networkMappings)))

	resp, err := runner.instance.Runtime.Start(ctx, &runtimev0.StartRequest{OtherNetworkMappings: networkMappings})
	if err != nil {
		return nil, w.Wrapf(err, "cannot start service instance")
	}

	if resp.Status.State != runtimev0.StartStatus_STARTED {
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
				return
			default:
				info, err := runner.instance.Runtime.Information(ctx, &runtimev0.InformationRequest{})
				if err != nil {
					w.Debug("cannot get information", wool.ErrField(err))
					return
				}
				if info.DesiredState.Stage != runtimev0.DesiredState_NOOP {
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
					w.Debug("sending action", wool.Field("action", action.Type))
					err = runner.playbook.Seed(ctx, action)
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
