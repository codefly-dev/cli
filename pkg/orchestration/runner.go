package orchestration

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/codefly-dev/cli/pkg/cli"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/wool"
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
	// Network
	networkMappings []*basev0.NetworkMapping

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
	outputPropertyForTest  *RunnerTestManager

	stopped chan struct{}

	runtimeContext string

	// Path fixture Name
	fixture string

	// Output environment variables
	outputEnv string

	// Running remote
	remoteEnvironment *resources.Environment
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
		outputPropertyForTest:  NewRunnerTestManager(instance.Unique()),

		stopped: make(chan struct{}, 1),
	}
	return runner, nil
}

func (runner *Runner) Load(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("Runner.Load", wool.ThisField(runner.instance))

	// This is a first iteration, it's more complicated that this: when Deploy
	// We should save the endpoint
	// But since we don't do much here
	// Init is an issue..Load is just setup

	env, err := runner.world.Env.Proto()
	if err != nil {
		return nil, w.Wrapf(err, "cannot get environment")
	}

	runner.instance.Runtime.Workspace = runner.world.Workspace

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
		wool.Field("endpoints", resources.MakeManyEndpointSummary(resp.Endpoints)))

	runner.endpoints = resp.Endpoints

	err = runner.world.SharedState.RecordEndpoints(ctx, runner.instance.Identity, resp.Endpoints)
	if err != nil {
		return nil, w.Wrapf(err, "cannot record endpoints")
	}

	err = runner.outputPropertyForLoad.Set(ctx, &RunnerLoadOutput{Endpoints: resp.Endpoints})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for load")
	}

	return runner.outputPropertyForLoad.Process(ctx)
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
	w := wool.Get(ctx).In("Runner.Init", wool.ThisField(runner.instance))

	if runner.remoteEnvironment != nil {
		return runner.InitRemote(ctx)

	}

	dependenciesEndpoints, err := runner.world.SharedState.GetDependenciesEndpoints(ctx, runner.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot run init")
	}

	conf, err := runner.world.ConfigurationManager.GetServiceConfiguration(ctx, runner.instance.Identity)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get service configuration")
	}

	workspaceConfigurations, err := runner.world.ConfigurationManager.GetWorkspaceDependenciesConfigurations(ctx, runner.instance.Service.WorkspaceConfigurationDependencies...)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get project configurations")
	}

	dependenciesConfigurations, err := runner.world.SharedState.GetDependentConfigurationsFor(ctx, runner.instance.Identity)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get configuration for dependencies")
	}

	networkMappings, err := runner.world.LocalNetworkManager.GenerateNetworkMappings(ctx, runner.world.Env, runner.world.Workspace, runner.instance.Identity, runner.endpoints)
	if err != nil {
		return nil, w.Wrapf(err, "cannot generate network mappings for service endpoints")
	}

	w.Debug("configuration",
		wool.Field("network mappings", resources.MakeManyNetworkMappingSummary(networkMappings)),
		wool.Field("service configuration", resources.MakeConfigurationSummary(conf)),
		wool.Field("dependencies endpoints", resources.MakeManyEndpointSummary(dependenciesEndpoints)),
		wool.Field("project configurations", resources.MakeManyConfigurationSummary(workspaceConfigurations)),
		wool.Field("dependencies configurations", resources.MakeManyConfigurationSummary(dependenciesConfigurations)))

	runtimeContext, err := resources.NewRuntimeContext(runner.runtimeContext)
	if err != nil {
		return nil, w.Wrapf(err, "cannot create runtime context: <%s>", runner.runtimeContext)
	}
	req := &runtimev0.InitRequest{
		RuntimeContext:             runtimeContext,
		ProposedNetworkMappings:    networkMappings,
		DependenciesEndpoints:      dependenciesEndpoints,
		Configuration:              conf,
		WorkspaceConfigurations:    workspaceConfigurations,
		DependenciesConfigurations: dependenciesConfigurations,
	}
	err = resources.Validate(req)
	if err != nil {
		return nil, w.Wrapf(err, "cannot validate init request")
	}
	resp, err := runner.instance.Runtime.Init(ctx, req)
	if err != nil {
		if ContextCancelled(err) {
			return nil, nil
		}
		return nil, w.Wrapf(err, "cannot call init")
	}

	if resp.Status != nil && resp.Status.State != runtimev0.InitStatus_READY {
		w.Debug("init failed: waiting")
		err = runner.outputPropertyForInit.Set(ctx, &RunnerInitOutput{failing: true})
		return runner.outputPropertyForInit.Process(ctx)
	}

	runner.networkMappings = resp.NetworkMappings

	err = runner.world.SharedState.RecordNetworkMappings(ctx, runner.instance.Service, networkMappings)
	if err != nil {
		return nil, w.Wrapf(err, "cannot record network mappings")
	}

	err = runner.world.ConfigurationManager.ExposeConfiguration(ctx, runner.instance.Identity, resp.RuntimeConfigurations...)
	if err != nil {
		return nil, w.Wrapf(err, "cannot record shared configuration infos")
	}

	if runner.outputEnv != "" {
		err = AppendEnvironmentVariablesToFile(ctx, runner.outputEnv, resp.RuntimeConfigurations)
		if err != nil {
			return nil, w.Wrapf(err, "cannot write environment variables to file")
		}
	}

	w.Debug("init", wool.Field("configuration info", resources.MakeManyConfigurationSummary(resp.RuntimeConfigurations)))

	err = runner.outputPropertyForInit.Set(ctx, &RunnerInitOutput{networkMappings: networkMappings, configurations: resp.RuntimeConfigurations})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for init")
	}
	outputProperty, err := runner.outputPropertyForInit.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for init")
	}

	return outputProperty, nil
}

func (runner *Runner) InitRemote(ctx context.Context) (*OutputProperty, error) {
	// This is a first iteration, it's more complicated that this: when Deploy
	// We should save the exposed configuration and networking and get it here
	// It won't work from ProposedNetworking != response Networking
	// With a remote environment
	// We only need to setup Networking
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(runner.instance))
	networkMappings, err := runner.world.LocalNetworkManager.GenerateNetworkMappings(ctx, runner.world.Env, runner.world.Workspace, runner.instance.Identity, runner.endpoints)
	if err != nil {
		return nil, w.Wrapf(err, "cannot generate network mappings for service endpoints")
	}
	runner.networkMappings = networkMappings

	err = runner.world.SharedState.RecordNetworkMappings(ctx, runner.instance.Service, networkMappings)
	if err != nil {
		return nil, w.Wrapf(err, "cannot record network mappings")
	}
	err = runner.outputPropertyForInit.Set(ctx, &RunnerInitOutput{networkMappings: networkMappings})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for init")
	}
	w.Info("done with remote init")
	return runner.outputPropertyForInit.Process(ctx)
}

func (runner *Runner) Start(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(runner.instance))
	w.Debug("start")

	if runner.remoteEnvironment != nil {
		return runner.StartRemote(ctx)
	}

	err := runner.StopIfNeeded(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot stopAfter service instance if needed")
	}

	// Build the request
	dependenciesNetworkMappings, err := runner.world.SharedState.GetDependenciesNetworkMappings(ctx, runner.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	req := &runtimev0.StartRequest{
		DependenciesNetworkMappings: dependenciesNetworkMappings,
		Fixture:                     runner.fixture,
	}
	err = resources.Validate(req)
	if err != nil {
		return nil, w.Wrapf(err, "cannot validate start request")
	}

	resp, err := runner.instance.Runtime.Start(ctx, req)

	if err != nil {
		if ContextCancelled(err) {
			return nil, nil
		}
		return nil, w.Wrapf(err, "context cancelled: cannot stopAfter service instance")
	}

	if resp.Status != nil && resp.Status.State != runtimev0.StartStatus_STARTED {
		return nil, w.NewError("service instance is not started")
	}

	err = runner.outputPropertyForStart.Set(ctx, &RunnerStartOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for start")
	}

	outputProperty, err := runner.outputPropertyForStart.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for start")
	}

	runner.isStarted = true
	return outputProperty, nil
}

func (runner *Runner) StartRemote(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(runner.instance))
	err := runner.world.RemoteNetworkManager.Expose(ctx, runner.remoteEnvironment, runner.world.Workspace, runner.instance.Identity, runner.endpoints, runner.networkMappings, cli.GetLogger())
	if err != nil {
		return nil, w.Wrapf(err, "cannot expose service")
	}
	outputProperty, err := runner.outputPropertyForStart.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for start")
	}

	runner.isStarted = true
	return outputProperty, nil
}

func (runner *Runner) Test(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(runner.instance))
	w.Debug("test")

	err := runner.StopIfNeeded(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot stopAfter service instance if needed")
	}

	req := &runtimev0.TestRequest{}
	err = resources.Validate(req)
	if err != nil {
		return nil, w.Wrapf(err, "cannot validate start request")
	}

	resp, err := runner.instance.Runtime.Test(ctx, req)

	if err != nil {
		if ContextCancelled(err) {
			return nil, nil
		}
		return nil, w.Wrapf(err, "got error from test request")
	}

	if resp.Status != nil && resp.Status.State != runtimev0.TestStatus_SUCCESS {
		return nil, w.NewError("service instance testing failed")
	}

	err = runner.outputPropertyForTest.Set(ctx, &RunnerTestOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for start")
	}

	outputProperty, err := runner.outputPropertyForTest.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for start")
	}

	runner.isStarted = true
	return outputProperty, nil
}

func (runner *Runner) StopIfNeeded(ctx context.Context) error {
	w := wool.Get(ctx).In("service.StopIfNeeded", wool.ThisField(runner.instance))
	w.Debug("stopIfNeeded", wool.Field("isStarted", runner.isStarted), wool.Field("isHotReloading", runner.instance.Runtime.IsHotReloading))
	if !runner.isStarted {
		return nil
	}
	if runner.instance.Runtime.IsHotReloading {
		return nil
	}

	_, err := runner.Stop(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot stopAfter service: %s", runner.Unique())
	}

	return nil

}

func (runner *Runner) Stop(ctx context.Context) (*OutputProperty, error) {
	if runner == nil {
		return &OutputProperty{}, nil
	}
	w := wool.Get(ctx).In("service.RunnerDoStop", wool.ThisField(runner.instance))
	w.Debug("stopping")
	// Build the request
	runner.isStarted = false
	go func() {
		runner.stopped <- struct{}{}
	}()
	stoppingContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := runner.instance.Runtime.Stop(stoppingContext, &runtimev0.StopRequest{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot stop service instance: %s", runner.Unique())
	}
	return &OutputProperty{}, nil
}

func (runner *Runner) Destroy(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.RunnerDoStop", wool.ThisField(runner.instance))
	w.Debug("shutting down")
	stoppingContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := runner.instance.Runtime.Destroy(stoppingContext, &runtimev0.DestroyRequest{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot shutdown service instance: %s", runner.Unique())
	}
	return &OutputProperty{}, nil
}

// Follow calls the agent for information and generate a channel of events for the service:
// - Handle restart
func (runner *Runner) Follow(ctx context.Context) error {
	w := wool.Get(ctx).In("service.Follow", wool.ThisField(runner.instance))
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runner.stopped:
				if !runner.restart {
					return
				}
			case <-ticker.C:
				info, err := runner.instance.Runtime.Information(ctx, &runtimev0.InformationRequest{})
				if err != nil {
					if !ContextCancelled(err) {
						w.Debug("cannot get information", wool.ErrField(err))
					}
					return
				}
				if info.DesiredState != nil && info.DesiredState.Stage != runtimev0.DesiredState_NOOP {
					w.Debug("received a request to change SharedState", wool.Field("SharedState", info.DesiredState.Stage))
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
					w.Trace("sending action", wool.Field("action", action.Type))
					err = runner.callback(ctx, action)
					if err != nil {
						w.Error("cannot seed", wool.ErrField(err))
						return
					}
				}
			}
		}
	}()
	return nil
}

func (runner *Runner) Unique() string {
	return runner.instance.Identity.Unique()
}

func (runner *Runner) WithRuntimeContext(runtimeContext string) {
	if runner == nil {
		return
	}
	runner.runtimeContext = runtimeContext
}

func (runner *Runner) WithFixture(fixture string) {
	if runner == nil {
		return
	}
	runner.fixture = fixture
}

func (runner *Runner) WithOutputEnv(path string) {
	if runner == nil {
		return
	}
	runner.outputEnv = path
}

func (runner *Runner) WithRemote(environment *resources.Environment) {
	runner.remoteEnvironment = environment
}

func AppendEnvironmentVariablesToFile(ctx context.Context, filePath string, confs []*basev0.Configuration) error {
	w := wool.Get(ctx).In("resources.AppendToFile", wool.Field("filePath", filePath))
	// filter out for native
	filtered := resources.FilterConfigurations(confs, resources.NewRuntimeContextNative())
	m := resources.NewEnvironmentVariableManager()
	err := m.AddConfigurations(ctx, filtered...)
	if err != nil {
		return w.Wrapf(err, "cannot add configurations")
	}
	// Open the file in append mode, create it if it doesn't exist
	file, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return w.Wrapf(err, "cannot open file")
	}
	defer file.Close()

	// Write each environment variable to the file
	allEnvs, err := m.All()
	if err != nil {
		return w.Wrapf(err, "cannot get environment variables")
	}
	for _, env := range allEnvs {
		_, err := file.WriteString(fmt.Sprintf("%s=%v\n", env.Key, env.Value))
		if err != nil {
			return w.Wrapf(err, "cannot write to file")
		}
	}
	return nil
}
