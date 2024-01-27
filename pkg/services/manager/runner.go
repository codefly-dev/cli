package manager

import (
	"context"
	"time"

	"github.com/codefly-dev/cli/pkg/services"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
	"github.com/codefly-dev/core/wool"
)

/*
Runner is a wrapper around a service instance to fit the outputProperty interface

- collects events from the agent API
- collects events from the service instance observability
*/
type Runner struct {
	instance *services.Instance

	actions *ActionManager

	// Requires
	requires []string

	// outputProperty managers
	outputPropertyForLoad  *RunnerLoadManager
	outputPropertyForInit  *RunnerInitManager
	outputPropertyForStart *RunnerStartManager
	world                  *World
}

func NewRunner(ctx context.Context, instance *services.Instance, world *World, actions *ActionManager) (*Runner, error) {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(instance))
	dependents, err := world.dependencies.DirectRequires(ctx, instance.Unique())
	if err != nil {
		return nil, w.Wrapf(err, "cannot get direct requires")
	}
	var uniques []string
	for _, dependent := range dependents {
		uniques = append(uniques, dependent.Unique)
	}
	w.Debug("requires", wool.Field("requires", uniques))
	runner := &Runner{
		instance:               instance,
		requires:               uniques,
		world:                  world,
		actions:                actions,
		outputPropertyForLoad:  NewRunnerLoadManager(instance.Unique()),
		outputPropertyForInit:  NewRunnerInitManager(instance.Unique()),
		outputPropertyForStart: NewRunnerStartManager(instance.Unique()),
	}
	return runner, nil
}

func (runner *Runner) Load(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(runner.instance.Service))

	resp, err := runner.instance.Runtime.Load(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}
	err = runner.outputPropertyForLoad.Set(ctx, &RunnerLoadOutput{Endpoints: resp.Endpoints})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for load")
	}
	outputProperty, err := runner.outputPropertyForLoad.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for load")
	}
	w.Debug("outputProperty", wool.Field("outputProperty", outputProperty))
	return outputProperty, nil
}

func (runner *Runner) Init(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(runner.instance.Service))
	// Build the request

	env, err := runner.world.env.Proto()
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	// provider information
	var infos []*basev0.ProviderInformation
	for _, req := range runner.instance.ProviderDependencies {
		info, err := runner.world.provider.GetProjectProviderInformation(ctx, req)
		if err != nil {
			return nil, w.Wrapf(err, "cannot get provider information")
		}
		infos = append(infos, info)
	}

	_, err = runner.instance.Runtime.Init(ctx, &runtimev0.InitRequest{
		Environment:   env,
		ProviderInfos: infos,
	})
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	err = runner.outputPropertyForInit.Set(ctx, &RunnerInitOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for load")
	}

	outputProperty, err := runner.outputPropertyForInit.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for load")
	}

	w.Debug("outputProperty", wool.Field("outputProperty", outputProperty))
	return outputProperty, nil
}

func (runner *Runner) Start(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(runner.instance.Service))
	w.Focus("start")
	// Build the request

	_, err := runner.instance.Runtime.Start(ctx, &runtimev0.StartRequest{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot start service instance")
	}
	err = runner.outputPropertyForStart.Set(ctx, &RunnerStartOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for load")
	}
	outputProperty, err := runner.outputPropertyForLoad.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for load")
	}
	w.Debug("outputProperty", wool.Field("outputProperty", outputProperty))
	return outputProperty, nil
}

func (runner *Runner) Stop(ctx context.Context) error {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(runner.instance.Service))
	// Build the request

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
			info, err := runner.instance.Runtime.Information(ctx, &runtimev0.InformationRequest{})
			w.Trace("info", wool.ResponseField(info))
			if err != nil {
				w.Error("cannot get information", wool.ErrField(err))
				return
			}
			if info.DesiredState.Stage != runtimev0.DesiredState_NOOP {
				w.Focus("received a request to change state", wool.Field("state", info.DesiredState.Stage))
				action := Action{Service: runner.Unique()}
				switch info.DesiredState.Stage {
				case runtimev0.DesiredState_LOAD:
					action.Type = RuntimeLoad
				case runtimev0.DesiredState_INIT:
					action.Type = RuntimeInit
				case runtimev0.DesiredState_START:
					action.Type = RuntimeStart
				}
				w.Focus("send action", wool.Field("action", action))
				runner.actions.Send(ctx, action)
			}
			time.Sleep(1000 * time.Millisecond)
		}
	}()
	return nil
}

func (runner *Runner) Unique() string {
	return runner.instance.Service.Unique()
}
