package orchestration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/codefly-dev/cli/pkg/cli"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/services"
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
	// Network
	networkMappings []*basev0.NetworkMapping

	// View of the world
	world *World

	// Callback
	callback Callback

	// failureSink, if set, is invoked when Follow observes a StartStatus_ERROR
	// after the service was previously up. Used by Flow to abort `codefly run`
	// when a child process dies (mind os.Exit(1), agent crash, etc.) instead
	// of leaking the parent + sibling plugins indefinitely.
	failureSink func(unique, msg string)

	// Requires
	requires []string

	// outputProperty hub.
	// isStarted and restart are written by Stop/Start/Init handlers from one
	// goroutine AND read by the Follow ticker goroutine. Use atomic.Bool
	// so the race detector stays quiet and Follow sees a fresh value on the
	// next tick regardless of scheduler ordering.
	isStarted atomic.Bool
	restart   atomic.Bool

	outputPropertyForLoad  *RunnerLoadManager
	outputPropertyForInit  *RunnerInitManager
	outputPropertyForStart *RunnerStartManager
	outputPropertyForTest  *RunnerTestManager

	stopped chan struct{}

	runtimeContext string

	// Path fixture Name
	fixture string

	// Per-service runtime overrides (KEY=VAL) injected into this service's process.
	overrides map[string]string

	// Output environment variables
	outputEnv string

	// Running remote
	remoteEnvironment *resources.Environment

	// testRequest carries CLI-provided test filtering / suite / extra-args
	// to the agent's Test RPC. Set by Flow only on the origin runner in
	// TestMode; nil for dependency runners.
	testRequest *runtimev0.TestRequest

	// testResponse holds the structured result of the last Test RPC so the
	// CLI can render counts / failed cases and set an accurate exit code.
	// Populated on both success and failure (whenever the RPC itself returns
	// a response, even if tests failed).
	testResponse *runtimev0.TestResponse
}

// WithTestRequest stores the TestRequest to forward to the agent on Test().
// Wired from CLI flags via Flow.WithTestRequest.
func (runner *Runner) WithTestRequest(req *runtimev0.TestRequest) {
	runner.testRequest = req
}

// TestResponse returns the structured response from the last Test RPC, or nil
// if this runner has not been tested (or the RPC failed before responding).
func (runner *Runner) TestResponse() *runtimev0.TestResponse {
	return runner.testResponse
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

// ContextDeadlineExceeded reports whether err represents a deadline-exceeded
// condition, either as a plain context.DeadlineExceeded or as a gRPC status
// with codes.DeadlineExceeded.
func ContextDeadlineExceeded(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if grpcErr, ok := status.FromError(err); ok {
		return grpcErr.Code() == codes.DeadlineExceeded
	}
	return false
}

func (runner *Runner) Init(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("Runner.Init", wool.ThisField(runner.instance))

	if runner.remoteEnvironment != nil {
		return runner.InitRemote(ctx)

	}

	// Configuration reads can block on a stalled provider (e.g. a dependency
	// service that failed to export its config). Bound each read with a
	// timeout so a single bad provider doesn't stall the entire Init.
	cfgCtx, cfgCancel := context.WithTimeout(ctx, 30*time.Second)
	defer cfgCancel()

	dependenciesEndpoints, err := runner.world.SharedState.GetDependenciesEndpoints(cfgCtx, runner.instance.Service)
	if err != nil {
		if ContextDeadlineExceeded(err) || ContextDeadlineExceeded(cfgCtx.Err()) {
			w.Warn("timeout waiting for dependency endpoints after 30s; check that dependency services are reachable")
			return nil, w.Wrapf(err, "init timeout: dependencies endpoints not available within 30s")
		}
		return nil, w.Wrapf(err, "cannot get dependencies endpoints")
	}

	conf, err := runner.world.ConfigurationManager.GetServiceConfiguration(cfgCtx, runner.instance.Identity)
	if err != nil {
		if ContextDeadlineExceeded(err) || ContextDeadlineExceeded(cfgCtx.Err()) {
			w.Warn("timeout waiting for service configuration after 30s; check that dependency services are reachable")
			return nil, w.Wrapf(err, "init timeout: service configuration not available within 30s")
		}
		return nil, w.Wrapf(err, "cannot get service configuration")
	}

	workspaceConfigurations, err := runner.world.ConfigurationManager.GetWorkspaceDependenciesConfigurations(cfgCtx, runner.instance.Service.WorkspaceConfigurationDependencies...)
	if err != nil {
		if ContextDeadlineExceeded(err) || ContextDeadlineExceeded(cfgCtx.Err()) {
			w.Warn("timeout waiting for workspace dependencies configurations after 30s; check that dependency services are reachable")
			return nil, w.Wrapf(err, "init timeout: workspace dependencies configurations not available within 30s")
		}
		return nil, w.Wrapf(err, "cannot get project configurations")
	}

	dependenciesConfigurations, err := runner.world.SharedState.GetDependentConfigurationsFor(cfgCtx, runner.instance.Identity)
	if err != nil {
		if ContextDeadlineExceeded(err) || ContextDeadlineExceeded(cfgCtx.Err()) {
			w.Warn("timeout waiting for dependencies configurations after 30s; check that dependency services are reachable")
			return nil, w.Wrapf(err, "init timeout: dependencies configurations not available within 30s")
		}
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
		Overrides:                   runner.overrides,
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

	runner.isStarted.Store(true)
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

	runner.isStarted.Store(true)
	return outputProperty, nil
}

func (runner *Runner) Test(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(runner.instance))
	w.Debug("test")

	err := runner.StopIfNeeded(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot stopAfter service instance if needed")
	}

	req := runner.testRequest
	if req == nil {
		req = &runtimev0.TestRequest{}
	}
	err = resources.Validate(req)
	if err != nil {
		return nil, w.Wrapf(err, "cannot validate start request")
	}

	w.Info("dispatching Test RPC",
		wool.Field("target", req.Target),
		wool.Field("suite", req.Suite),
		wool.Field("filters", req.Filters),
		wool.Field("extra_args", req.ExtraArgs))

	resp, err := runner.instance.Runtime.Test(ctx, req)

	if err != nil {
		if ContextCancelled(err) {
			return nil, nil
		}
		return nil, w.Wrapf(err, "got error from test request")
	}

	// Keep the structured response so the CLI can render counts / failed
	// cases and exit with an accurate code, regardless of pass/fail.
	runner.testResponse = resp

	if resp.GetStatus() != nil && resp.GetStatus().GetState() != runtimev0.TestStatus_SUCCESS {
		return nil, w.NewError("tests failed for %s: %s", runner.Unique(), summarizeTestResponse(resp))
	}

	err = runner.outputPropertyForTest.Set(ctx, &RunnerTestOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for start")
	}

	outputProperty, err := runner.outputPropertyForTest.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for start")
	}

	runner.isStarted.Store(true)
	return outputProperty, nil
}

func (runner *Runner) StopIfNeeded(ctx context.Context) error {
	w := wool.Get(ctx).In("service.StopIfNeeded", wool.ThisField(runner.instance))
	w.Debug("stopIfNeeded", wool.Field("isStarted", runner.isStarted.Load()), wool.Field("isHotReloading", runner.instance.Runtime.IsHotReloading))
	if !runner.isStarted.Load() {
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
	// Info-level so a user watching `codefly run service` shutdown
	// sees WHICH service is being stopped, not just a silent hang.
	w.Info(fmt.Sprintf("stopping %s", runner.Unique()))
	start := time.Now()
	// Clear the restart flag first — without this, a restart that was
	// requested but not yet honored could perpetually resurrect the
	// service on the next Follow tick.
	runner.restart.Store(false)
	runner.isStarted.Store(false)
	// Non-blocking signal to Follow. A blocking goroutine-send here would
	// leak if Follow had already exited via ctx.Done, and a second Stop
	// call would try to send to a channel already saturated by the first.
	select {
	case runner.stopped <- struct{}{}:
	case <-ctx.Done():
	default:
	}
	stoppingContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := runner.instance.Runtime.Stop(stoppingContext, &runtimev0.StopRequest{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot stop service instance: %s", runner.Unique())
	}
	w.Info(fmt.Sprintf("stopped %s in %s", runner.Unique(), time.Since(start).Round(time.Millisecond)))
	return &OutputProperty{}, nil
}

// Destroy shuts down the agent. Calls Stop first if the runner is still
// marked started — callers that have already called Stop are unaffected
// (the isStarted guard makes the inner Stop a no-op), but callers that
// jump straight to Destroy get a clean two-phase shutdown instead of
// force-killing the agent mid-flight.
func (runner *Runner) Destroy(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.RunnerDoStop", wool.ThisField(runner.instance))
	if runner.isStarted.Load() {
		if _, err := runner.Stop(ctx); err != nil {
			w.Warn("stop before destroy failed — proceeding anyway", wool.ErrField(err))
		}
	}
	w.Debug("shutting down")
	stoppingContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := runner.instance.Runtime.Destroy(stoppingContext, &runtimev0.DestroyRequest{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot shutdown service instance: %s", runner.Unique())
	}
	return &OutputProperty{}, nil
}

// restartActionType maps the stage the agent asks to restart at into the
// action to re-seed. A watch event fires a START request, but for compiled
// services the compile happens during configure (Init): re-running only Start
// would relaunch the stale binary and never surface a fresh compiler error.
// So a START request re-enters at Init, which the policy cascades back into
// Start.
func restartActionType(stage runtimev0.DesiredState_Stage) ActionType {
	switch stage {
	case runtimev0.DesiredState_LOAD:
		return RuntimeLoad
	case runtimev0.DesiredState_INIT, runtimev0.DesiredState_START:
		return RuntimeInit
	default:
		return ""
	}
}

// Follow calls the agent for information and generate a channel of events for the service:
// - Handle restart
// - Detect runner death (StartStatus → ERROR) and report up via failureSink
func (runner *Runner) Follow(ctx context.Context) error {
	w := wool.Get(ctx).In("service.Follow", wool.ThisField(runner.instance))
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				// Flow context cancelled (e.g. SIGINT, shutdown); exit so the
				// goroutine doesn't outlive its caller.
				return
			case <-runner.stopped:
				if !runner.restart.Load() {
					return
				}
			case <-ticker.C:
				info, err := runner.instance.Runtime.Information(ctx, &runtimev0.InformationRequest{})
				if err != nil {
					if ContextCancelled(err) {
						return
					}
					// gRPC call failed and we're not shutting down — the agent
					// plugin itself is gone or unreachable. Report up so the
					// parent can tear the rest of the tree down.
					w.Debug("cannot get information", wool.ErrField(err))
					if runner.isStarted.Load() && runner.failureSink != nil {
						runner.failureSink(runner.Unique(), fmt.Sprintf("agent unreachable: %v", err))
					}
					return
				}

				// Detect that the underlying runner process died after a
				// successful start. The plugin sets StartStatus → ERROR via
				// MarkRunnerExited; we mirror that into a flow-level failure
				// so `codefly run` can shut down the rest of the stack.
				if runner.isStarted.Load() && info.StartStatus != nil &&
					info.StartStatus.State == runtimev0.StartStatus_ERROR &&
					runner.failureSink != nil {
					msg := info.StartStatus.Message
					if msg == "" {
						msg = "runner exited"
					}
					w.Warn("runner died after start", wool.Field("message", msg))
					runner.failureSink(runner.Unique(), msg)
					return
				}

				if info.DesiredState != nil && info.DesiredState.Stage != runtimev0.DesiredState_NOOP {
					w.Debug("received a request to change SharedState", wool.Field("SharedState", info.DesiredState.Stage))
					action := Action{Service: runner.Unique(), Type: restartActionType(info.DesiredState.Stage)}
					// Tear down the current child process tree before re-seeding.
					// Without this, the agent's previous run (next dev / go build /
					// python / etc. and all of its workers) stays alive while a
					// fresh Load/Init/Start is driven in parallel. The pgid
					// tree-kill in NativeProc.Stop only runs when Runtime.Stop is
					// invoked, so silent restarts via DesiredState used to leak
					// the entire pool each round — which is how a single flaky
					// service multiplied into a fork bomb under `codefly run`.
					if runner.isStarted.Load() {
						stopCtx, stopCancel := context.WithTimeout(ctx, 10*time.Second)
						if _, stopErr := runner.Stop(stopCtx); stopErr != nil {
							w.Warn("stop-before-restart failed — proceeding anyway", wool.ErrField(stopErr))
						}
						stopCancel()
					}
					runner.restart.Store(true)
					w.Trace("sending action", wool.Field("action", action.Type))
					// Bound the callback so a stalled playbook consumer can't
					// indefinitely block the Follow ticker (which would keep
					// the agent gRPC conn warm and prevent shutdown).
					cbCtx, cbCancel := context.WithTimeout(ctx, 5*time.Second)
					err = runner.callback(cbCtx, action)
					cbCancel()
					if err != nil {
						// Clear restart so we don't re-drive the same failing
						// action on the next tick. If the action is legit-needed
						// a human or the runtime will re-request it.
						runner.restart.Store(false)
						w.Error("cannot seed", wool.ErrField(err))
						if runner.failureSink != nil {
							runner.failureSink(runner.Unique(),
								fmt.Sprintf("restart action failed: %v", err))
						}
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

// SupportsNix reports whether the agent advertises the nix runtime among its
// RuntimeRequirements — i.e. the service can run Docker-free via nix. Used to
// gate the free→nix fallback: a service that supports nix can fall back when
// Docker is unavailable, one that does not can only run under Docker.
func (runner *Runner) SupportsNix() bool {
	if runner == nil || runner.instance == nil || runner.instance.Info == nil {
		return false
	}
	for _, r := range runner.instance.Info.RuntimeRequirements {
		if r.Type == agentv0.Runtime_NIX {
			return true
		}
	}
	return false
}

func (runner *Runner) WithFixture(fixture string) {
	if runner == nil {
		return
	}
	runner.fixture = fixture
}

func (runner *Runner) WithOverrides(overrides map[string]string) {
	if runner == nil {
		return
	}
	runner.overrides = overrides
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
