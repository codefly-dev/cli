package orchestration

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/agents/contract"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	gopsnet "github.com/shirou/gopsutil/v3/net"
	"github.com/shirou/gopsutil/v3/process"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

	// outputProperty hub.
	// isStarted is written by Stop/Start handlers and read by Follow.
	isStarted atomic.Bool

	// everStarted latches on the first successful Start and is never cleared.
	// isStarted cannot answer "has this service ever run", because Stop clears
	// it before a hot reload re-enters Init.
	everStarted atomic.Bool

	restartMu       sync.Mutex
	pendingRestart  ActionType
	restartInFlight bool

	outputPropertyForLoad  *RunnerLoadManager
	outputPropertyForInit  *RunnerInitManager
	outputPropertyForStart *RunnerStartManager
	outputPropertyForTest  *RunnerTestManager

	stopped chan struct{}

	runtimeContext string

	// containerRecoveryIdentity is the ownership acknowledgement this runner's
	// flow projected, set by Flow.configureRunner.
	containerRecoveryIdentity string

	// Path fixture Name
	fixture string

	// Per-service runtime overrides (KEY=VAL) injected into this service's process.
	overrides map[string]string

	// Output environment variables
	outputEnv string

	// Running remote
	remoteEnvironment *environments.Environment

	// testRequest carries CLI-provided test filtering / suite / extra-args
	// to the agent's Test RPC. Set by Flow only on the origin runner in
	// TestMode; nil for dependency runners.
	testRequest *runtimev0.TestRequest

	// testResponse holds the structured result of the last Test RPC so the
	// CLI can render counts / failed cases and set an accurate exit code.
	// Populated on both success and failure (whenever the RPC itself returns
	// a response, even if tests failed).
	testResponse *runtimev0.TestResponse

	// serviceRunningForTest preserves an explicitly started target for suites
	// whose advertised dependency mode is START_STACK.
	serviceRunningForTest bool

	// testSkipped records that the agent advertises no test capability, so no
	// Test RPC was dispatched. Read by Flow so the CLI and the CI report can
	// show a skip instead of an unearned pass.
	testSkipped bool
}

// WithTestRequest stores the TestRequest to forward to the agent on Test().
// Wired from CLI flags via Flow.WithTestRequest.
func (runner *Runner) WithTestRequest(req *runtimev0.TestRequest) {
	runner.testRequest = req
}

func (runner *Runner) WithServiceRunningForTest(running bool) {
	runner.serviceRunningForTest = running
}

// TestResponse returns the structured response from the last Test RPC, or nil
// if this runner has not been tested (or the RPC failed before responding).
func (runner *Runner) TestResponse() *runtimev0.TestResponse {
	return runner.testResponse
}

// TestSkipped reports whether Test returned without dispatching an RPC because
// the agent advertises no test capability.
func (runner *Runner) TestSkipped() bool {
	return runner.testSkipped
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
		if runner.outputPropertyForLoad.processed == nil {
			return nil, w.Wrapf(err, "cannot load runtime instance for %s", runner.instance.Unique())
		}
		err = runner.outputPropertyForLoad.Set(ctx, &RunnerLoadOutput{Err: err.Error()})
		if err != nil {
			return nil, w.Wrapf(err, "cannot set outputProperty for load")
		}
		return runner.outputPropertyForLoad.Process(ctx)
	}

	if resp == nil || resp.Status == nil {
		return nil, w.NewError("cannot load runtime instance for %s: agent returned no status", runner.instance.Unique())
	}
	if resp.Status.State != runtimev0.LoadStatus_READY {
		message := statusDiagnostic(resp.Status.Message, "agent reported load failure")
		w.Warn(fmt.Sprintf("load failed for %s: %s", runner.instance.Unique(), message))
		if runner.outputPropertyForLoad.processed == nil {
			return nil, w.NewError("cannot load runtime instance for %s: %s", runner.instance.Unique(), message)
		}
		err = runner.outputPropertyForLoad.Set(ctx, &RunnerLoadOutput{Err: message})
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
	if err := runner.validateContainerRecovery(); err != nil {
		return nil, err
	}

	// Reject stale native listeners before calling the agent's Init hook.
	// Infrastructure agents are allowed to bind their assigned endpoint during
	// Init (for example Vault starts its Docker/Nix runtime there), so probing in
	// Start is already too late and mistakes the service we just initialized for
	// a stale process.
	//
	// A running service being hot-reloaded keeps its port and is marked started,
	// so it must skip this first-start guard.
	if err := runner.checkInitialPortAvailability(ctx); err != nil {
		return nil, w.Wrapf(err, "cannot initialize %s", runner.instance.Unique())
	}

	// Configuration reads can block on a stalled provider (e.g. a dependency
	// service that failed to export its config). Bound each read with a
	// timeout so a single bad provider doesn't stall the entire Init.
	cfgCtx, cfgCancel := context.WithTimeout(ctx, 30*time.Second)
	defer cfgCancel()

	inputs, err := runner.gatherInitInputs(ctx, cfgCtx, w)
	if err != nil {
		return nil, initReadError(cfgCtx, w, err, "dependencies endpoints", "dependencies endpoints")
	}
	dependenciesNetworkMappings, err := runner.world.SharedState.GetDependenciesNetworkMappings(cfgCtx, runner.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get initialized dependency network mappings")
	}
	if runner.testRequest != nil && !runner.serviceRunningForTest && len(dependenciesNetworkMappings) > 0 &&
		!slices.Contains(runner.instance.Info.GetContract().GetCapabilities(), contract.RuntimeInitDependencyMappings) {
		return nil, w.NewError("dependency-only tests require agent capability %s to consume accepted addresses at Init", contract.RuntimeInitDependencyMappings)
	}

	conf, err := runner.world.ConfigurationManager.GetServiceConfiguration(cfgCtx, runner.instance.Identity)
	if err != nil {
		return nil, initReadError(cfgCtx, w, err, "service configuration", "service configuration")
	}

	runtimeContext, err := resources.NewRuntimeContext(runner.runtimeContext)
	if err != nil {
		return nil, w.Wrapf(err, "cannot create runtime context: <%s>", runner.runtimeContext)
	}

	workspaceConfigurations, err := runner.world.workspaceConfigurationsFor(cfgCtx, runner.instance.Service,
		dependenciesNetworkMappings, resources.NetworkAccessFromRuntimeContext(runtimeContext))
	if err != nil {
		return nil, initReadError(cfgCtx, w, err, "workspace dependencies configurations", "project configurations")
	}

	dependenciesConfigurations, err := runner.world.SharedState.GetDependentConfigurationsFor(cfgCtx, runner.instance.Identity)
	if err != nil {
		return nil, initReadError(cfgCtx, w, err, "dependencies configurations", "configuration for dependencies")
	}

	networkMappings, err := runner.world.LocalNetworkManager.GenerateNetworkMappings(ctx, runner.world.Env.Runtime(), runner.world.Workspace, runner.instance.Identity, runner.endpoints, runtimeContext)
	if err != nil {
		return nil, w.Wrapf(err, "cannot generate network mappings for service endpoints")
	}

	w.Debug("configuration",
		wool.Field("network mappings", resources.MakeManyNetworkMappingSummary(networkMappings)),
		wool.Field("service configuration", resources.MakeConfigurationSummary(conf)),
		wool.Field("dependencies endpoints", resources.MakeManyEndpointSummary(dependenciesEndpoints)),
		wool.Field("project configurations", resources.MakeManyConfigurationSummary(workspaceConfigurations)),
		wool.Field("dependencies configurations", resources.MakeManyConfigurationSummary(dependenciesConfigurations)))

	// Init is the only lifecycle call guaranteed to reach a service under test:
	// a test policy replaces the origin's Start with a barrier or skips it
	// altogether, so a fixture or an override delivered only at Start reaches
	// every dependency and never the service the suite is about. Start still
	// carries both, and the agent takes the first non-empty.
	req := &runtimev0.InitRequest{
		RuntimeContext:              runtimeContext,
		ProposedNetworkMappings:     networkMappings,
		DependenciesEndpoints:       dependenciesEndpoints,
		DependenciesNetworkMappings: dependenciesNetworkMappings,
		Configuration:               conf,
		WorkspaceConfigurations:     workspaceConfigurations,
		DependenciesConfigurations:  dependenciesConfigurations,
		Fixture:                     runner.fixture,
		Overrides:                   runner.runtimeOverridesFor(networkMappings),
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

	if resp == nil || resp.Status == nil {
		return nil, w.NewError("cannot initialize %s: agent returned no status", runner.instance.Unique())
	}
	if resp.Status.State != runtimev0.InitStatus_READY {
		return runner.failedInit(ctx, w, resp.Status.Message)
	}
	return runner.recordInit(ctx, w, runtimeContext, conf, workspaceConfigurations, networkMappings, resp)
}

// initReadError names a bounded Init read's failure: a stalled provider is
// reported as the timeout it is, anything else as what it is.
func initReadError(cfgCtx context.Context, w *wool.Wool, err error, timeoutWhat, what string) error {
	if ContextDeadlineExceeded(err) || ContextDeadlineExceeded(cfgCtx.Err()) {
		w.Warn(fmt.Sprintf("timeout waiting for %s after 30s; check that dependency services are reachable", timeoutWhat))
		return w.Wrapf(err, "init timeout: %s not available within 30s", timeoutWhat)
	}
	return w.Wrapf(err, "cannot get %s", what)
}

// failedInit reports the agent's initialization failure: as the error when
// nothing consumes the init output, else as a failing output.
func (runner *Runner) failedInit(ctx context.Context, w *wool.Wool, statusMessage string) (*OutputProperty, error) {
	message := statusDiagnostic(statusMessage, "agent reported initialization failure")
	w.Warn(fmt.Sprintf("initialization failed for %s: %s", runner.instance.Unique(), message))
	if runner.outputPropertyForInit.processed == nil {
		return nil, w.NewError("cannot initialize %s: %s", runner.instance.Unique(), message)
	}
	if err := runner.outputPropertyForInit.Set(ctx, &RunnerInitOutput{failing: true}); err != nil {
		return nil, w.Wrapf(err, "cannot set failed init output for %s", runner.instance.Unique())
	}
	return runner.outputPropertyForInit.Process(ctx)
}

// recordInit publishes what the agent accepted — its network mappings, its
// runtime configurations and, when requested, the process environment file —
// and produces the init output.
func (runner *Runner) recordInit(ctx context.Context, w *wool.Wool, runtimeContext *basev0.RuntimeContext, conf *basev0.Configuration, workspaceConfigurations []*basev0.Configuration, networkMappings []*basev0.NetworkMapping, resp *runtimev0.InitResponse) (*OutputProperty, error) {
	// The agent, not the proposal, decides the addresses this service serves.
	// The validated accepted set is the only one published: runner, shared
	// state and the exported environment must never hold different views of
	// where this service can be reached.
	accepted, err := acceptNetworkMappings(ctx, runner.instance.Identity, networkMappings, resp.NetworkMappings)
	if err != nil {
		return nil, w.Wrapf(err, "cannot accept network mappings from %s", runner.instance.Unique())
	}
	runner.networkMappings = accepted

	err = runner.world.SharedState.RecordNetworkMappings(ctx, runner.instance.Service, accepted)
	if err != nil {
		return nil, w.Wrapf(err, "cannot record network mappings")
	}

	err = runner.world.ConfigurationManager.ExposeConfiguration(ctx, runner.instance.Identity, resp.RuntimeConfigurations...)
	if err != nil {
		return nil, w.Wrapf(err, "cannot record shared configuration infos")
	}

	if runner.outputEnv != "" {
		// Dependency configurations are intentionally omitted here: they are not
		// final until the dependency barrier and are written once at Start from
		// the refreshed shared state. Writing them at Init too would duplicate
		// every dependency secret in the owner-only file (and record stale
		// pre-barrier values). Own, workspace, and this service's own runtime
		// configurations are final at Init, so they are written now.
		err = AppendServiceProcessConfigurationsToFile(
			ctx,
			runner.outputEnv,
			runtimeContext,
			conf,
			workspaceConfigurations,
			nil,
			resp.RuntimeConfigurations,
		)
		if err != nil {
			return nil, w.Wrapf(err, "cannot write environment variables to file")
		}
	}

	w.Debug("init", wool.Field("configuration info", resources.MakeManyConfigurationSummary(resp.RuntimeConfigurations)))

	err = runner.outputPropertyForInit.Set(ctx, &RunnerInitOutput{networkMappings: accepted, configurations: resp.RuntimeConfigurations})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for init")
	}
	outputProperty, err := runner.outputPropertyForInit.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for init")
	}

	return outputProperty, nil
}

// setRunProducers records the run set: the services a flow starts or deploys,
// by <module>/<service>, and its origin.
func (world *World) setRunProducers(required []string, origin *resources.Service) {
	world.runProducers = make(map[string]bool, len(required)+1)
	for _, unique := range required {
		world.runProducers[unique] = true
	}
	if origin != nil {
		world.runProducers[resources.WithUnique(origin).Unique()] = true
	}
}

// producerInRun is the run set as core asks for it: nil before the flow has
// decided one, so no reference is provably to a producer of the run.
func (world *World) producerInRun() func(unique string) bool {
	if world.runProducers == nil {
		return nil
	}
	return func(unique string) bool { return world.runProducers[unique] }
}

// workspaceConfigurationsFor resolves the workspace configurations one service
// receives. ${endpoint:…} references resolve against that service's dependency
// mappings, in the address family of its access, plus the mappings of every
// producer in the run that a configuration the service declares names (see
// referencedProducerMappings).
func (world *World) workspaceConfigurationsFor(
	ctx context.Context, service *resources.Service,
	dependencyMappings []*basev0.NetworkMapping, access *basev0.NetworkAccess,
) ([]*basev0.Configuration, error) {
	dependencies := make([]string, 0, len(service.WorkspaceConfigurationDependencies))
	for _, dependency := range service.WorkspaceConfigurationDependencies {
		if !world.excludedWorkspaceConfigurations[dependency] {
			dependencies = append(dependencies, dependency)
		}
	}
	referenced, err := world.referencedProducerMappings(ctx, service, dependencies, dependencyMappings)
	if err != nil {
		return nil, err
	}
	mappings := append(slices.Clone(dependencyMappings), referenced...)
	manager := world.ConfigurationManager.ForConsumer(mappings, access).WithRunProducers(world.producerInRun())
	declared, err := manager.GetWorkspaceDependenciesConfigurations(ctx, dependencies...)
	if err != nil {
		return nil, err
	}
	// The composition root injects its own workspace configurations into every
	// service, so a composed-module service resolves root-provided values
	// without redeclaring them as dependencies. A service that declares a
	// dependency on one of the root's own configurations (e.g. the root service
	// itself) yields that name in both sets, so union by name to avoid emitting
	// it twice.
	root, err := manager.GetCompositionRootWorkspaceConfigurations(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(declared))
	for _, conf := range declared {
		for _, info := range conf.Infos {
			seen[info.Name] = true
		}
	}
	out := declared
	for _, conf := range root {
		if world.workspaceConfigurationExcluded(conf) || world.workspaceConfigurationSeen(conf, seen) {
			continue
		}
		out = append(out, conf)
	}
	return out, nil
}

// referencedProducerMappings returns the network mappings of the producers the
// workspace configurations a service declares reference by ${endpoint:…}, for
// every referenced endpoint the service's own dependency mappings (have) do not
// already carry.
//
// A workspace configuration is the composition root's: it names endpoints the
// consuming module cannot know, such as the host by the composition's name for
// it, which is why the consumer declares the group and not the dependency. The
// root's reference is what binds the two, so it resolves for the consumer when
// the producer is part of this run.
//
// A declared dependency does not by itself carry the referenced endpoint: a
// `kind: external` one is never started or waited for, so its producer has
// recorded nothing when the consumer reads the group, and a declaration may
// name other endpoints of the producer than the one referenced. Only an
// endpoint already in have is skipped. The service's own endpoints resolve the
// same way. A reference whose producer is not a service of the workspace never
// reaches here: core's check over the effective set
// (checkEffectiveWorkspaceConfigurationReferences) has already refused it by
// name.
//
// The groups considered are the EFFECTIVE set — the declared ones and the
// composition root's alike — because a root group's reference is exactly what
// #882 was about: the root supplies a value every service can read, and binding
// only the declared groups' producers deletes it from every service that did not
// declare the group. `declared` is still passed, and is not a second selection:
// it is what separates a reference the dependency graph ORDERS from one it does
// not, which is what decides whether a failure to derive is fatal.
//
// Failing to derive a producer's addresses is an error wherever an address must
// already exist — every reference in a render, and a declared group's reference
// in a run — because swallowing it leaves the consumer's read to fail with core's
// "producer is not part of the run", which names the wrong cause and buries the
// real one in a log line nobody correlates. A root group's reference in a RUN is
// the one exception, and it is a drop rather than a swallow: nothing ordered the
// producer, so there is nothing to blame, and
// refuseDroppedWorkspaceConfigurationValues warns by name.
func (world *World) referencedProducerMappings(
	ctx context.Context, service *resources.Service, declared, effective []string, have []*basev0.NetworkMapping,
	withheld withheldCredentials,
) ([]*basev0.NetworkMapping, error) {
	if world == nil || world.ConfigurationManager == nil || world.SharedState == nil || world.Dependencies == nil || len(effective) == 0 {
		return nil, nil
	}
	// Whether failing to derive a producer's addresses is fatal depends on
	// whether anything guarantees the address exists yet, and that is a question
	// about the MODE first and the group second.
	//
	// In a render, nothing is early: a deployed address is a pure function of the
	// producer's identity and namespace, so a failure to derive one is a fault
	// whatever group named it. Every reference is fatal there. (An earlier
	// revision of this made root-only references non-fatal in every mode while
	// fixing the run, which left a render swallowing the same failure it had
	// refused the commit before — the regression this comment exists to stop
	// coming back.)
	//
	// In a run, only a reference in a group the service DECLARES is ordered:
	// core's graph puts that producer before the consumer
	// (ServiceDependencies.addConfigurationReferenceEdges), so a failure there is
	// a real fault. A root group's reference is ordered by nothing, so the same
	// failure is the run simply being early — fatal would break every run of a
	// composition whose root group holds a reference, which is what
	// --temporary-ports guarantees (localProducerMappings cannot know an address
	// allocated at initialization). Those are dropped, and
	// refuseDroppedWorkspaceConfigurationValues is where the drop is warned
	// about.
	fatal := make(map[string]bool)
	if !world.deploys() {
		for _, reference := range world.ConfigurationManager.WorkspaceEndpointReferences(declared...) {
			fatal[reference] = true
		}
	}
	// A reference carried ONLY by values this service does not receive is not
	// discovered for it: a withheld credential's producer does not have to have
	// a derivable address, and treating it as fatal in a render refused the
	// render of a service over a value it was deliberately not given.
	// (Layer-4 round-five F4.)
	skip := withheld.skips(world.referencingWorkspaceConfigurationValues(effective))
	producers, err := world.workspaceProducers(ctx)
	if err != nil {
		return nil, err
	}
	consumerModule := ""
	if identity, idErr := service.Identity(); idErr == nil {
		consumerModule = identity.Module
	}
	var out []*basev0.NetworkMapping
	collected := make(map[string]bool)
	for _, reference := range world.ConfigurationManager.WorkspaceEndpointReferences(effective...) {
		if skip[reference] {
			continue
		}
		info, err := resources.ParseEndpoint(reference)
		if err != nil {
			continue
		}
		producer := info.Module + "/" + info.Service
		// Whether the bound set already carries this reference's own endpoint
		// is asked of core's selection, over the producer's manifest. Asking it
		// here, with a second implementation of the same rule, is what let an
		// API sibling count as the named endpoint and leave discovery skipping
		// a producer whose endpoint was never bound.
		exactName := false
		if producers != nil {
			if declaring, ok := producers(producer); ok && declaring != nil {
				if selected, selErr := resources.SelectEndpointForReference(consumerModule, info, declaring.Endpoints); selErr == nil {
					exactName = selected.ExactName
				}
			}
		}
		if collected[producer] || mappingsCarry(have, info, exactName) {
			continue
		}
		collected[producer] = true
		mappings, err := world.producerNetworkMappings(ctx, producer)
		if err != nil {
			if !world.deploys() && !fatal[reference] {
				// The producer is named, the REFERENCE is not. A producer here
				// is a <module>/<service> this package resolved against the
				// workspace's own manifest, so naming it publishes nothing
				// from the value; the reference's text is the value, and a log
				// line is the easiest place for a value to leak — written
				// whether or not anyone is watching, kept, and shipped.
				wool.Get(ctx).In("World.referencedProducerMappings").Warn(
					"a workspace configuration value will be missing for this service: the composition root's group references a producer whose address cannot be derived yet, and a root group's reference orders nothing",
					wool.Field("consumer", consumerLabel(service)), wool.Field("producer", producer),
					wool.Field("reason", err.Error()))
				continue
			}
			return nil, fmt.Errorf("cannot derive the addresses of %s, which a workspace configuration reference that %s declares names: %w",
				producer, consumerLabel(service), err)
		}
		visible, err := world.exportableTo(ctx, service, mappings)
		if err != nil {
			return nil, err
		}
		out = append(out, visible...)
	}
	return out, nil
}

// exportableTo keeps the mappings whose endpoint the consumer's module may
// reach, by core's own export rule (resources.ValidateEndpointVisibility — the
// one body behind every "may this consumer depend on this endpoint" answer),
// judged against the producer's MANIFEST.
//
// Binding a producer's mappings is not the same act as checking a reference, and
// that is the whole reason this exists. The check
// (configurations.checkEndpointReference) returns on the FIRST manifest endpoint
// a reference matches; the resolution (resources.resolveEndpointReference)
// returns the first BOUND mapping that matches AND has an instance for the
// consumer's network access, falling through to the next match otherwise. A
// reference naming an API rather than an endpoint name — or a name that is
// another endpoint's API — matches several of a producer's endpoints, so the two
// can land on different endpoints: the check passes on the public one, and the
// resolution hands over the private one's address, either because it is bound
// first or because the public one has no instance for this consumer's access. An
// earlier revision of this file claimed the opposite ("the same function
// reaching the same verdict… the two cannot disagree"); it was false, and the
// layer-5 review reproduced it.
//
// Filtering what is bound closes it from the consumer's side: an endpoint this
// module may not reach is not in the set, so there is nothing to fall through
// to. It is core's rule, not a second one, and it narrows only: within one
// module ValidateEndpointVisibility returns nil, so a service reading its own
// module's endpoints is untouched.
//
// It is applied to the WHOLE set a resolution walks — the consumer's own
// dependency mappings as well as the ones producer discovery binds. Filtering
// only the second was a bypass, not a fix: a bare service dependency (one naming
// no endpoints) is handed every mapping its producer published
// (StateManager.GetDependenciesNetworkMappings, which narrows by the dependency's
// endpoint list and not by visibility), and when those already carry a match
// discovery never re-binds the producer at all — so the filter never ran and the
// interpolation walked the unfiltered set. Reported and reproduced as layer-5
// round-five NEW-1.
//
// Visibility is read from the producer's manifest through the memoized
// workspaceProducers lookup, NOT from the mapping. In a run the mapping's
// endpoint is the agent's Load answer, recorded without being checked against
// the manifest, so judging from it would let a runtime agent reporting a private
// endpoint as public reopen the hole, and one that drops `allow-modules` refuse a
// legitimate `internal` reference. Core's check reads the manifest; so does this.
// (Layer-5 round-five NEW-6.)
//
// It cannot cost a consumer a value it was entitled to, with one exception worth
// stating rather than implying. Every reference in the effective set has already
// been validated against the same manifest by core's own check
// (checkEffectiveWorkspaceConfigurationReferences), so a reference naming an
// endpoint this filter removes has already been refused — EXCEPT where the
// reference's exact name is unreachable for this consumer and a sibling sharing
// its API is not: core's check passes on the sibling, this filter removes only
// the unreachable name, and the value then resolves to the sibling instead of
// saying the endpoint asked for is private. That is a known gap, carried as a
// follow-up; the right answer is to refuse with ValidateEndpointVisibility's own
// reason. Otherwise the only mappings this can remove are ones no reference the
// consumer receives may name.
// What it does change, deliberately, is a cross-module BARE dependency: an
// address of the producer's private endpoint used to be interpolatable into a
// workspace configuration value, and is not any more. Disclosed in
// `docs/commands.md`.
//
// The durable fix belongs in core, in the two functions above: judge visibility
// for EVERY endpoint a reference can match (or refuse a reference that matches
// more than one as ambiguous), and resolve only to the endpoint that was judged;
// and let the dependency mappings a consumer is handed be what
// PermittedDependencyEndpoints grants. Named in docs/orchestration.md and in the
// PR body.
//
// A visibility this core does not know is treated as refusing, because
// ValidateEndpointVisibility refuses it — `module` is not that case (it is a
// deprecated alias for internal with every module allowed, so it permits).
func (world *World) exportableTo(
	ctx context.Context, consumer *resources.Service, mappings []*basev0.NetworkMapping,
) ([]*basev0.NetworkMapping, error) {
	if len(mappings) == 0 {
		return mappings, nil
	}
	producers, err := world.workspaceProducers(ctx)
	if err != nil {
		return nil, err
	}
	if producers == nil {
		// No workspace, so no manifest to judge against. A resolution carrying a
		// reference is already refused for that reason
		// (checkEffectiveWorkspaceConfigurationReferences), so what reaches here
		// resolves no reference and has nothing to filter.
		return mappings, nil
	}
	// An unidentifiable consumer gets the strict answer rather than a lenient
	// one: "" matches no producer module, so only endpoints visible to every
	// module survive. NewFlow resolves nothing without an identity, so this is a
	// floor, not a path.
	consumerModule := ""
	if identity, err := consumer.Identity(); err == nil {
		consumerModule = identity.Module
	}
	out := make([]*basev0.NetworkMapping, 0, len(mappings))
	for _, mapping := range mappings {
		endpoint := mapping.GetEndpoint()
		if endpoint == nil {
			continue
		}
		unique := endpoint.GetModule() + "/" + endpoint.GetService()
		declared, ok := manifestEndpoint(producers, unique, endpoint)
		if !ok {
			wool.Get(ctx).In("World.exportableTo").Debug(
				"not binding a mapping for an endpoint the producer's manifest does not declare",
				wool.Field("consumer", consumerLabel(consumer)),
				wool.Field("endpoint", resources.EndpointDestination(endpoint)))
			continue
		}
		// The declared Location travels with the rest of the declaration: core
		// judges the whole declaration before it judges the consumer, so a
		// location the model does not define is ErrInvalidEndpointDeclaration
		// rather than something this filter silently treats as reachable.
		if err := resources.ValidateEndpointVisibility(consumerModule, endpoint.GetModule(), endpoint.GetService(),
			declared.Name, resources.Visibility(declared.Visibility), declared.Location, declared.AllowModules); err != nil {
			wool.Get(ctx).In("World.exportableTo").Debug(
				"not binding a producer endpoint this consumer's module may not reach",
				wool.Field("consumer", consumerLabel(consumer)),
				wool.Field("endpoint", resources.EndpointDestination(endpoint)),
				wool.Field("reason", err.Error()))
			continue
		}
		out = append(out, mapping)
	}
	return out, nil
}

// manifestEndpoint finds the endpoint a mapping stands for in its producer's
// manifest, by CANONICAL IDENTITY: its name, which core refuses to let a service
// declare twice, with the mapping's API checked for consistency against it.
//
// A mapping that carries no name is not identified and not bound, and that is
// the point rather than a conservatism. An earlier revision fell back to
// matching by API, which does not establish which endpoint a mapping
// represents: with a public `api` and a private `admin` both on api `rest`, a
// mapping of `{Name: "", Api: "rest"}` whose instance addresses `admin` was
// judged against `api` — the first API match — approved as public, and then
// returned admin's address by core's interpolator, which matches the retained
// mapping by API. The visibility verdict has to be about the endpoint whose
// address is in the mapping, so the mapping has to say which endpoint that is.
// (Layer-2 round-six finding 2.)
//
// A mapping whose API disagrees with the manifest endpoint of that name is not
// bound either: the two fields would then describe different endpoints, and
// nothing here can say which one the address belongs to.
func manifestEndpoint(
	producers configurations.ProducerLookup, unique string, endpoint *basev0.Endpoint,
) (*resources.Endpoint, bool) {
	if endpoint.GetName() == "" {
		return nil, false
	}
	producer, ok := producers(unique)
	if !ok || producer == nil {
		return nil, false
	}
	for _, declared := range producer.Endpoints {
		if declared == nil || declared.Name != endpoint.GetName() {
			continue
		}
		// The API has to agree too, in both directions: a mapping that omits it
		// for an endpoint the manifest gives one is as unidentified as one that
		// contradicts it. A real mapping carries the manifest's own endpoint
		// proto (acceptNetworkInstances keeps the proposal's), so this only ever
		// rejects a mapping nothing in the composition published.
		if declared.API != endpoint.GetApi() {
			return nil, false
		}
		return declared, true
	}
	return nil, false
}

// mappingsCarry reports whether mappings already hold the endpoint a reference
// names, so discovery does not re-bind its producer.
//
// requireExactName says the reference has an endpoint named exactly as its
// token, which the consumer may reach. Then only a mapping carrying THAT name
// counts: an API-only match is not the endpoint the reference names, and
// treating it as one made discovery skip the producer, so the named endpoint's
// mapping was never bound — withExactNamePrecedence then stripped the API-only
// sibling as the wrong answer and the value was dropped with a WARN blaming the
// producer's address, or refused in a render. The same shape silently resolved
// to the sibling before precedence existed. (Layer-5 round-seven N2.)
func mappingsCarry(mappings []*basev0.NetworkMapping, info *resources.EndpointInformation, requireExactName bool) bool {
	for _, mapping := range mappings {
		endpoint := mapping.GetEndpoint()
		if endpoint == nil || endpoint.GetModule() != info.Module || endpoint.GetService() != info.Service {
			continue
		}
		// The same match a reference resolves by (resources.InterpolateEndpoints).
		if info.API != "" && endpoint.GetApi() != info.API {
			continue
		}
		if requireExactName {
			if endpoint.GetName() == info.Name {
				return true
			}
			continue
		}
		if info.Name == "" || endpoint.GetName() == info.Name || endpoint.GetApi() == info.Name {
			return true
		}
	}
	return false
}

// localProducerMappings derives a producer's mappings as a local run's Init
// proposes them. Only legitimate while the proposal IS the address: a named port
// is a pure function of the producer's identity, so deriving it for a producer
// of this run that has not initialized yet yields the address it will serve.
//
// Under --temporary-ports it is not: every call takes a fresh ephemeral port
// from the kernel, so a derived address is a free port nothing listens on, and
// two consumers of one group would each get a different one. Deriving there
// would hand out a plausible address for a service that is not behind it, which
// fails at connect time far from its cause — so say so instead. The producer
// initializing first makes its recorded mappings authoritative and never reaches
// this — but only a reference in a group the consumer DECLARES is ordered that
// way (core's ServiceDependencies.addConfigurationReferenceEdges reads a
// consumer's declared groups and no more), so a root group's reference can reach
// here legitimately. referencedProducerMappings drops that case rather than
// failing the run.
func (world *World) localProducerMappings(ctx context.Context, service *resources.Service, identity *resources.ServiceIdentity, endpoints []*basev0.Endpoint) ([]*basev0.NetworkMapping, error) {
	if world.LocalNetworkManager == nil || world.runtimeContextFor == nil {
		return nil, nil
	}
	if world.temporaryPorts {
		return nil, fmt.Errorf("its address is allocated at initialization (--temporary-ports), so this run cannot know it before %s initializes: order it before this service, or declare it as a dependency for the referenced endpoint", identity.Unique())
	}
	runtimeContext, err := resources.NewRuntimeContext(world.runtimeContextFor(service))
	if err != nil {
		return nil, err
	}
	return world.LocalNetworkManager.GenerateNetworkMappings(ctx, world.Env.Runtime(), world.Workspace, identity, endpoints, runtimeContext)
}

// producerNetworkMappings returns a producer's mappings: the ones it recorded,
// or, before it has, the ones its own Init (or deploy) will propose.
//
// Where the proposal comes from differs by mode, because what makes it
// authoritative differs. A deployed producer's in-cluster address is a pure
// function of its identity and namespace, so it holds whether or not this
// operation deploys that producer, and its manifest's endpoints are enough. A
// local producer's address is allocated, so only a producer of THIS run has one
// to derive: membership is exactly "it recorded endpoints at Load", every
// service of a run being loaded before any initializes. A producer that never
// loaded is not in the run and has no address — it contributes nothing, and core
// fails the consumer's read naming the key and the producer rather than this
// handing back an address no service is behind.
func (world *World) producerNetworkMappings(ctx context.Context, producer string) ([]*basev0.NetworkMapping, error) {
	if mappings, ok := world.SharedState.GetNetworkMappingsFromUnique(producer); ok {
		return mappings, nil
	}
	service, err := world.Dependencies.ServiceFromUnique(producer)
	if err != nil {
		return nil, nil
	}
	identity, err := service.Identity()
	if err != nil {
		return nil, err
	}
	if world.deploys() {
		endpoints, err := service.LoadEndpoints(ctx)
		if err != nil {
			return nil, err
		}
		if len(endpoints) == 0 {
			return nil, nil
		}
		return world.remoteProducerMappings(ctx, identity, endpoints)
	}
	endpoints := world.SharedState.RecordedEndpoints(producer)
	if len(endpoints) == 0 {
		return nil, nil
	}
	return world.localProducerMappings(ctx, service, identity, endpoints)
}

// deploys reports whether this world reaches producers at their deployed
// in-cluster addresses rather than at locally allocated ones.
func (world *World) deploys() bool {
	switch world.Mode {
	case DeployMode, SnapshotMode, BuildMode, SyncMode:
		return true
	default:
		return false
	}
}

// workspaceConfigurationSeen reports whether every Info name in a resolved
// workspace configuration is already present in seen (all Infos of a workspace
// configuration share one name), i.e. the configuration was already emitted via
// the declared-dependency set.
func (world *World) workspaceConfigurationSeen(conf *basev0.Configuration, seen map[string]bool) bool {
	for _, info := range conf.Infos {
		if seen[info.Name] {
			return true
		}
	}
	return false
}

// workspaceConfigurationExcluded reports whether a resolved workspace
// configuration is profile-excluded. Workspace configurations carry their name
// on each Info (the Configuration.Origin is always "workspace"), and every Info
// in a given configuration shares that name.
func (world *World) workspaceConfigurationExcluded(conf *basev0.Configuration) bool {
	for _, info := range conf.Infos {
		if world.excludedWorkspaceConfigurations[info.Name] {
			return true
		}
	}
	return false
}

func (flow *Flow) WorkspaceConfigurationsFor(ctx context.Context, service *resources.Service) ([]*basev0.Configuration, error) {
	if flow == nil || flow.world == nil {
		return nil, nil
	}
	runtimeContext, err := resources.NewRuntimeContext(flow.runtimeContextFor(service))
	if err != nil {
		return nil, err
	}
	dependencyMappings, err := flow.SharedState.GetDependenciesNetworkMappings(ctx, service)
	if err != nil {
		return nil, err
	}
	return flow.world.workspaceConfigurationsFor(ctx, service, dependencyMappings, resources.NetworkAccessFromRuntimeContext(runtimeContext))
}

func (runner *Runner) InitRemote(ctx context.Context) (*OutputProperty, error) {
	// This is a first iteration, it's more complicated that this: when Deploy
	// We should save the exposed configuration and networking and get it here
	// It won't work from ProposedNetworking != response Networking
	// With a remote environment
	// We only need to setup Networking
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(runner.instance))
	runtimeContext, err := resources.NewRuntimeContext(runner.runtimeContext)
	if err != nil {
		return nil, w.Wrapf(err, "cannot create runtime context: <%s>", runner.runtimeContext)
	}
	networkMappings, err := runner.world.LocalNetworkManager.GenerateNetworkMappings(ctx, runner.world.Env.Runtime(), runner.world.Workspace, runner.instance.Identity, runner.endpoints, runtimeContext)
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
	if runner.outputEnv != "" {
		runtimeContext, runtimeErr := resources.NewRuntimeContext(runner.runtimeContext)
		if runtimeErr != nil {
			return nil, w.Wrapf(runtimeErr, "cannot create output environment runtime context")
		}
		// Environment export needs the same identity fields the agent receives,
		// but it must also work for infrastructure agents whose optional path
		// fields are empty. ServiceIdentity.Proto validates those deployment
		// fields and is therefore intentionally not the conversion boundary.
		identity := &basev0.ServiceIdentity{
			Workspace:           runner.instance.Identity.Workspace,
			Module:              runner.instance.Identity.Module,
			Name:                runner.instance.Identity.Name,
			Version:             runner.instance.Identity.Version,
			WorkspacePath:       runner.instance.Identity.WorkspacePath,
			RelativeToWorkspace: runner.instance.Identity.RelativeToWorkspace,
		}
		// Core guards this same dereference before making the identical call
		// (services.RuntimeInstance.Start) and simply forwards the request
		// unnarrowed when the module is absent. This carrier cannot do that:
		// unnarrowed here means writing dependency addresses whose visibility
		// nothing can check. Without a consumer module there is no permitted
		// set to compute, so refuse to write rather than write unchecked.
		if runner.instance.Module == nil {
			return nil, w.NewError("cannot resolve dependency network mappings for output environment: %s has no module", runner.instance.Unique())
		}
		endpointMappings, mappingErr := outputEnvNetworkMappings(
			runner.instance.Module.Name,
			runner.instance.Service.ServiceDependencies,
			runner.networkMappings,
			dependenciesNetworkMappings,
		)
		if mappingErr != nil {
			return nil, w.Wrapf(mappingErr, "cannot resolve dependency network mappings for output environment")
		}
		// Dependency agents publish connection configurations during their Init.
		// Start is the single point that writes them to the output environment,
		// after the dependency barrier: the target's own Init may have observed
		// the shared configuration state before a concurrently initialized
		// dependency exposed its runtime values, so only here are they complete.
		dependencyConfigurations, dependencyErr := runner.world.SharedState.GetDependentConfigurationsFor(ctx, runner.instance.Identity)
		if dependencyErr != nil {
			return nil, w.Wrapf(dependencyErr, "cannot load dependency configurations for output environment")
		}
		err = AppendServiceProcessConfigurationsToFile(
			ctx,
			runner.outputEnv,
			runtimeContext,
			nil,
			nil,
			dependencyConfigurations,
			nil,
		)
		if err != nil {
			return nil, w.Wrapf(err, "cannot write dependency configurations to output environment")
		}
		if appendErr := AppendRuntimeEnvironmentToFile(
			ctx,
			runner.outputEnv,
			identity,
			runtimeContext,
			runner.fixture,
			runner.runtimeOverrides(),
			endpointMappings,
		); appendErr != nil {
			return nil, w.Wrapf(appendErr, "cannot write runtime environment variables to file")
		}
	}

	req := &runtimev0.StartRequest{
		DependenciesNetworkMappings: dependenciesNetworkMappings,
		Fixture:                     runner.fixture,
		Overrides:                   runner.runtimeOverrides(),
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
		return nil, w.Wrapf(err, "cannot start service instance")
	}

	if resp == nil || resp.Status == nil {
		return nil, w.NewError("cannot start %s: agent returned no status", runner.instance.Unique())
	}
	if resp.Status.State != runtimev0.StartStatus_STARTED {
		message := statusDiagnostic(resp.Status.Message, "agent reported start failure")
		return nil, w.NewError("cannot start %s: %s", runner.instance.Unique(), message)
	}

	err = runner.outputPropertyForStart.Set(ctx, &RunnerStartOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for start")
	}

	outputProperty, err := runner.outputPropertyForStart.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for start")
	}

	runner.markStarted()
	return outputProperty, nil
}

func (runner *Runner) markStarted() {
	runner.isStarted.Store(true)
	runner.everStarted.Store(true)
	runner.restartMu.Lock()
	runner.restartInFlight = false
	runner.restartMu.Unlock()
}

func (runner *Runner) queueRestart(actionType ActionType) {
	runner.restartMu.Lock()
	defer runner.restartMu.Unlock()
	if runner.pendingRestart == "" || actionType == RuntimeLoad {
		runner.pendingRestart = actionType
	}
}

func (runner *Runner) takePendingRestart() (ActionType, bool) {
	runner.restartMu.Lock()
	defer runner.restartMu.Unlock()
	if runner.pendingRestart == "" || runner.restartInFlight || !runner.isStarted.Load() {
		return "", false
	}
	actionType := runner.pendingRestart
	runner.pendingRestart = ""
	runner.restartInFlight = true
	return actionType, true
}

func (runner *Runner) cancelRestarts() {
	runner.restartMu.Lock()
	runner.pendingRestart = ""
	runner.restartInFlight = false
	runner.restartMu.Unlock()
}

// portHolderLookupTimeout bounds identifying who holds a clashing port.
// Enumerating the machine's listening sockets shells out to lsof on darwin,
// and lsof blocks indefinitely on an unreachable network mount — so an
// unbounded lookup would turn a legible port-collision error into a silent
// hang at Init. A diagnostic must never outlive the failure it explains: past
// this deadline the report simply carries no attribution.
const portHolderLookupTimeout = 3 * time.Second

// portCollision is one of the runner's own native endpoints that another
// process is already listening on.
type portCollision struct {
	port     uint32
	endpoint string
}

// boundNativePorts returns "<port> (<endpoint>)" descriptions for each of the
// runner's own native endpoint ports that another process is already listening
// on, naming the process that holds each one when it can be identified. A
// successful TCP dial is the honest signal that the port is held: unlike a
// listen probe it doesn't trip on TIME_WAIT sockets that have no listener.
func (runner *Runner) boundNativePorts(ctx context.Context) []string {
	var collisions []portCollision
	for _, mapping := range runner.networkMappings {
		// networkMappings arrive over the agent's Init gRPC response, so a nil
		// mapping or endpoint is possible at this boundary — skip rather than panic.
		if mapping == nil || mapping.Endpoint == nil {
			continue
		}
		instance := resources.FilterNetworkInstance(ctx, mapping.Instances, resources.NewNativeNetworkAccess())
		if instance == nil {
			continue
		}
		conn, err := net.DialTimeout("tcp", instance.Host, 200*time.Millisecond)
		if err != nil {
			continue
		}
		_ = conn.Close()
		collisions = append(collisions, portCollision{port: instance.Port, endpoint: mapping.Endpoint.Name})
	}
	if len(collisions) == 0 {
		return nil
	}
	lookupCtx, cancel := holderLookupContext(ctx)
	defer cancel()
	listeners := listeningPorts(lookupCtx)
	held := make([]string, 0, len(collisions))
	for _, collision := range collisions {
		held = append(held, fmt.Sprintf("%d (%s)%s", collision.port, collision.endpoint, portHolderSuffix(lookupCtx, listeners, collision.port)))
	}
	return held
}

// holderLookupContext derives the deadline the holder attribution runs under.
// One deadline covers the whole attribution — the socket enumeration and every
// executable lookup it feeds — so naming holders is bounded no matter how many
// endpoints clash. The caller's context carries no deadline of its own (it is
// the run context), which is exactly why this must impose one.
func holderLookupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, portHolderLookupTimeout)
}

// listeningPorts maps each locally LISTENing TCP port to the pid that holds it.
// Building the whole map once per probe keeps a stack-wide collision from
// re-enumerating the machine's sockets for every clashing endpoint.
func listeningPorts(ctx context.Context) map[uint32]int32 {
	connections, err := gopsnet.ConnectionsWithContext(ctx, "tcp")
	if err != nil {
		return map[uint32]int32{}
	}
	ports := make(map[uint32]int32, len(connections))
	for _, connection := range connections {
		if connection.Status != "LISTEN" || connection.Pid <= 0 {
			continue
		}
		// A port bound on several addresses (v4 + v6, or per-interface) yields
		// one connection each; they belong to the same listener, so first wins.
		if _, seen := ports[connection.Laddr.Port]; !seen {
			ports[connection.Laddr.Port] = connection.Pid
		}
	}
	return ports
}

// portHolderSuffix names the process squatting on port, so an "already in use"
// failure says which pid to look at instead of leaving the user to run lsof.
//
// It reports the holder's executable PATH and never its command line: the path
// is what identifies a stale codefly binary (".../.cache/native/<hash>"), while
// the arguments of an arbitrary process on this machine routinely carry
// credentials — a --db-url with a password, an --api-key — and this string is
// printed to the terminal and, in headless runs, into CI logs.
//
// It returns "" when the holder cannot be identified: socket ownership is not
// always readable (a listener owned by another user, a lookup that ran past
// the deadline), and a missing name must not cost us the port report itself.
func portHolderSuffix(ctx context.Context, listeners map[uint32]int32, port uint32) string {
	pid, ok := listeners[port]
	if !ok {
		return ""
	}
	proc, err := process.NewProcessWithContext(ctx, pid)
	if err != nil {
		return fmt.Sprintf(" held by pid %d", pid)
	}
	executable, err := proc.ExeWithContext(ctx)
	if err != nil || executable == "" {
		return fmt.Sprintf(" held by pid %d", pid)
	}
	return fmt.Sprintf(" held by pid %d (%s)", pid, executable)
}

func (runner *Runner) checkInitialPortAvailability(ctx context.Context) error {
	// Only the very first Init can see a ghost: once this runner has started
	// the service, a bound port is its own — either still running (hot reload)
	// or still releasing after the Stop that precedes a restart. Testing
	// isStarted alone missed the restart case, because Stop clears it before
	// the re-Init.
	if runner.isStarted.Load() || runner.everStarted.Load() {
		return nil
	}
	if held := runner.boundNativePorts(ctx); len(held) > 0 {
		// The holder is whoever bound the port first — it may be a leftover
		// codefly run, but it may equally be the user's own server, so the
		// remedy is stated conditionally rather than asserting a provenance
		// this check never verified.
		return fmt.Errorf("port %s already in use — stop the process holding it, or run 'codefly clear' if it is a leftover codefly run, then retry",
			strings.Join(held, ", "))
	}
	return nil
}

func statusDiagnostic(message, fallback string) string {
	if message = strings.TrimSpace(message); message != "" {
		return message
	}
	return fallback
}

func (runner *Runner) StartRemote(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(runner.instance))
	err := runner.world.RemoteNetworkManager.Expose(ctx, runner.remoteEnvironment, runner.world.Workspace, runner.instance.Identity, runner.endpoints, runner.networkMappings, runner.world.OutputSink)
	if err != nil {
		return nil, w.Wrapf(err, "cannot expose service")
	}
	outputProperty, err := runner.outputPropertyForStart.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for start")
	}

	runner.markStarted()
	return outputProperty, nil
}

func (runner *Runner) Test(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(runner.instance))
	w.Debug("test")
	advertised, supported := validationSupport(runner.instance.Info, RuntimeTest)
	if advertised && !supported {
		runner.testSkipped = true
		w.Info("test is explicitly unsupported by this agent; skipping")
		return OnInit(), nil
	}

	if !runner.serviceRunningForTest {
		err := runner.StopIfNeeded(ctx)
		if err != nil {
			return nil, w.Wrapf(err, "cannot stopAfter service instance if needed")
		}
	}

	req := runner.testRequest
	if req == nil {
		req = &runtimev0.TestRequest{}
	}
	err := resources.Validate(req)
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
		if status.Code(err) == codes.Unimplemented && advertised {
			return nil, w.NewError("validation contract violation for %s: test is advertised but Runtime.Test is unimplemented", runner.Unique())
		}
		return nil, w.Wrapf(err, "got error from test request")
	}

	// Keep the structured response so the CLI can render counts / failed
	// cases and exit with an accurate code, regardless of pass/fail.
	runner.testResponse = resp

	//nolint:staticcheck // SA1019 deliberately: the deprecated field is what
	// released runtime agents still populate, so reading the replacement would
	// silently see a zero value from every agent in the field — a test run that
	// reports success whatever happened. A migration needs the agent owners, and
	// nothing in CI would catch it. Tracked as a follow-up.
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

// Build asks the runtime agent for its native compile/typecheck phase. This is
// intentionally distinct from Builder.Build, which creates the deployable
// artifact (typically a container image).
func (runner *Runner) Build(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("Runner.Build", wool.ThisField(runner.instance))
	advertised, supported := validationSupport(runner.instance.Info, RuntimeBuild)
	if advertised && !supported {
		w.Info("native build is explicitly unsupported by this agent; skipping")
		return OnInit(), nil
	}
	resp, err := runner.instance.Runtime.Runtime.Build(ctx, &runtimev0.BuildRequest{})
	if status.Code(err) == codes.Unimplemented {
		if advertised {
			return nil, w.NewError("validation contract violation for %s: compile is advertised but Runtime.Build is unimplemented", runner.Unique())
		}
		w.Info("native build is not supported by this agent; skipping")
		return OnInit(), nil
	}
	if err != nil {
		return nil, w.Wrapf(err, "native build RPC failed")
	}
	if resp == nil {
		return nil, w.NewError("native build failed for %s: agent returned no response", runner.Unique())
	}
	if strings.TrimSpace(resp.Output) != "" {
		w.Forwardf("%s", resp.Output)
	}
	// A few existing agents return an empty response for an intentional no-op.
	// Preserve that compatibility until validation capabilities make the skip
	// explicit in AgentInformation.
	if resp.Status == nil {
		w.Info("native build completed with a legacy empty response")
		return OnInit(), nil
	}
	if resp.Status.State != runtimev0.BuildStatus_SUCCESS {
		message := statusDiagnostic(resp.Status.Message, resp.Output)
		return nil, w.NewError("native build failed for %s: %s", runner.Unique(), statusDiagnostic(message, "agent reported build failure"))
	}
	return OnInit(), nil
}

// Lint asks the runtime agent to run the service-native static checks. The
// agent—not the CI provider or CLI—owns the concrete language commands.
func (runner *Runner) Lint(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("Runner.Lint", wool.ThisField(runner.instance))
	advertised, supported := validationSupport(runner.instance.Info, RuntimeLint)
	if advertised && !supported {
		w.Info("lint is explicitly unsupported by this agent; skipping")
		return OnInit(), nil
	}
	resp, err := runner.instance.Runtime.Runtime.Lint(ctx, &runtimev0.LintRequest{})
	if status.Code(err) == codes.Unimplemented {
		if advertised {
			return nil, w.NewError("validation contract violation for %s: lint is advertised but Runtime.Lint is unimplemented", runner.Unique())
		}
		w.Info("lint is not supported by this agent; skipping")
		return OnInit(), nil
	}
	if err != nil {
		return nil, w.Wrapf(err, "lint RPC failed")
	}
	if resp == nil || resp.Status == nil {
		return nil, w.NewError("lint failed for %s: agent returned no status", runner.Unique())
	}
	if strings.TrimSpace(resp.Output) != "" {
		w.Forwardf("%s", resp.Output)
	}
	if resp.Status.State != runtimev0.LintStatus_SUCCESS {
		message := statusDiagnostic(resp.Status.Message, resp.Output)
		return nil, w.NewError("lint failed for %s: %s", runner.Unique(), statusDiagnostic(message, "agent reported lint failure"))
	}
	return OnInit(), nil
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
	runner.cancelRestarts()
	// Non-blocking signal to Follow. A blocking goroutine-send here would
	// leak if Follow had already exited via ctx.Done, and a second Stop
	// call would try to send to a channel already saturated by the first.
	select {
	case runner.stopped <- struct{}{}:
	case <-ctx.Done():
	default:
	}
	return runner.stop(ctx)
}

func (runner *Runner) stop(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.RunnerDoStop", wool.ThisField(runner.instance))
	// Info-level so a user watching `codefly run service` shutdown
	// sees WHICH service is being stopped, not just a silent hang.
	w.Info(fmt.Sprintf("stopping %s", runner.Unique()))
	start := time.Now()
	runner.isStarted.Store(false)
	// The RPC must have the phase's budget. A shorter 10s deadline races
	// the runtime's own bounded cleanup and abandons its final response.
	stoppingContext, cancel := context.WithTimeout(ctx, defaultStopPhaseBudget)
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
	stoppingContext, cancel := context.WithTimeout(ctx, defaultShutdownPhaseBudget)
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

func (runner *Runner) handleDesiredState(ctx context.Context, stage runtimev0.DesiredState_Stage) error {
	if actionType := restartActionType(stage); actionType != "" {
		runner.queueRestart(actionType)
	}

	actionType, ok := runner.takePendingRestart()
	if !ok {
		return nil
	}
	if runner.callback == nil {
		runner.cancelRestarts()
		return errors.New("restart requested before the orchestration callback was initialized")
	}

	stopCtx, stopCancel := context.WithTimeout(ctx, 10*time.Second)
	_, err := runner.stop(stopCtx)
	stopCancel()
	if err != nil {
		runner.cancelRestarts()
		return fmt.Errorf("stop before restart: %w", err)
	}

	action := Action{Service: runner.Unique(), Type: actionType}
	cbCtx, cbCancel := context.WithTimeout(ctx, 5*time.Second)
	err = runner.callback(cbCtx, action)
	cbCancel()
	if err != nil {
		runner.cancelRestarts()
		return fmt.Errorf("restart action failed: %w", err)
	}
	return nil
}

const runnerInformationTimeout = 5 * time.Second

func runnerInformationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, runnerInformationTimeout)
}

// Follow monitors the agent for service lifecycle events:
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
				return
			case <-ticker.C:
				infoCtx, cancel := runnerInformationContext(ctx)
				info, err := runner.instance.Runtime.Information(infoCtx, &runtimev0.InformationRequest{})
				cancel()
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
					w.Error("runner died after start", wool.Field("message", msg))
					runner.failureSink(runner.Unique(), msg)
					return
				}

				stage := runtimev0.DesiredState_NOOP
				if info.DesiredState != nil {
					stage = info.DesiredState.Stage
				}
				if stage != runtimev0.DesiredState_NOOP {
					w.Debug("received a request to change SharedState", wool.Field("SharedState", info.DesiredState.Stage))
				}
				if err := runner.handleDesiredState(ctx, stage); err != nil {
					w.Error("cannot restart service", wool.ErrField(err))
					if runner.failureSink != nil {
						runner.failureSink(runner.Unique(), err.Error())
					}
					return
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

// SupportsBackend reports whether the agent advertises the given execution
// backend among its SupportedBackends. The CLI uses this capability set to
// resolve the concrete runtime for a service launched with the "free" hint.
func (runner *Runner) SupportsBackend(backend agentv0.Backend_Type) bool {
	if runner == nil || runner.instance == nil || runner.instance.Info == nil {
		return false
	}
	for _, b := range runner.instance.Info.SupportedBackends {
		if b.Type == backend {
			return true
		}
	}
	return false
}

// SupportsNix reports whether the service can run Docker-free via nix — i.e. it
// advertises the NIX backend. Used to gate the free→nix fallback: a service that
// supports nix can fall back when Docker is unavailable, one that does not can
// only run under Docker.
func (runner *Runner) SupportsNix() bool {
	return runner.SupportsBackend(agentv0.Backend_NIX)
}

// SupportedBackends returns the plugin's execution backends in PREFERENCE ORDER
// ([0] = preferred). The CLI walks this to resolve a "free" service to the first
// backend actually available in the environment.
func (runner *Runner) SupportedBackends() []agentv0.Backend_Type {
	if runner == nil || runner.instance == nil || runner.instance.Info == nil {
		return nil
	}
	out := make([]agentv0.Backend_Type, 0, len(runner.instance.Info.SupportedBackends))
	for _, b := range runner.instance.Info.SupportedBackends {
		out = append(out, b.Type)
	}
	return out
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

// runtimeOverrides carries invocation-scoped runtime identity through the
// backwards-compatible StartRequest override seam. This makes naming-scope
// isolation available to already-published service agents; newer agents also
// derive the same carrier from Environment, so both paths converge on the
// identical value.
func (runner *Runner) runtimeOverrides() map[string]string {
	return runner.runtimeOverridesFor(runner.networkMappings)
}

// selfNetworkAccess is the access this runner's peers reach it with — its own
// runtime context's, as core's RuntimeWrapper.AddSelfEndpoints selects: a
// native process advertises its host address, a container its container-network
// address.
func (runner *Runner) selfNetworkAccess() *basev0.NetworkAccess {
	runtimeContext, err := resources.NewRuntimeContext(runner.runtimeContext)
	if err != nil {
		return resources.NewNativeNetworkAccess()
	}
	return resources.NetworkAccessFromRuntimeContext(runtimeContext)
}

// runtimeOverridesFor derives the runtime overrides against a given view of the
// service's own network mappings. Init has only the proposed mappings — the
// accepted set does not exist until the agent answers — and an agent keeps the
// first non-empty override set it receives, so the self-endpoint carrier must
// already ride Init rather than first appear at Start.
//
// The self carrier is layered beneath the caller's overrides: it is derived,
// and an explicit --set of the same key is the operator's to make. It rides the
// override seam so it reaches agents built before core emits it themselves
// (RuntimeWrapper.AddSelfEndpoints); the derivation is core's, so both paths
// converge on the same value.
func (runner *Runner) runtimeOverridesFor(own []*basev0.NetworkMapping) map[string]string {
	self := SelfEndpointEnvironmentVariables(context.Background(), own, runner.selfNetworkAccess())
	overrides := make(map[string]string, len(self)+len(runner.overrides)+1)
	for key, value := range self {
		overrides[key] = value
	}
	for key, value := range runner.overrides {
		overrides[key] = value
	}
	if runner.world != nil && runner.world.Env != nil && runner.world.Env.NamingScope != "" {
		overrides[resources.NamingScopePrefix] = runner.world.Env.NamingScope
	}
	return overrides
}

func (runner *Runner) WithOutputEnv(path string) {
	if runner == nil {
		return
	}
	runner.outputEnv = path
}

func (runner *Runner) WithRemote(environment *environments.Environment) {
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
	allEnvs, err := m.All()
	if err != nil {
		return w.Wrapf(err, "cannot get environment variables")
	}
	return appendEnvironmentVariablesToFile(ctx, filePath, allEnvs)
}

// AppendServiceProcessConfigurationsToFile exports the full configuration
// environment admitted to a service's Init request, plus configurations the
// agent produced during Init. This mirrors the service-agent process boundary:
// own and workspace configurations are admitted directly, dependency and
// runtime configurations are filtered to the selected backend. The output is
// owner-only because these inputs intentionally include provider credentials
// and dependency connection secrets.
func AppendServiceProcessConfigurationsToFile(
	ctx context.Context,
	filePath string,
	runtimeContext *basev0.RuntimeContext,
	serviceConfiguration *basev0.Configuration,
	workspaceConfigurations []*basev0.Configuration,
	dependencyConfigurations []*basev0.Configuration,
	runtimeConfigurations []*basev0.Configuration,
) error {
	w := wool.Get(ctx).In("resources.AppendServiceProcessConfigurationsToFile", wool.Field("filePath", filePath))
	manager := resources.NewEnvironmentVariableManager()
	if err := manager.AddConfigurations(ctx, serviceConfiguration); err != nil {
		return w.Wrapf(err, "cannot add service configuration")
	}
	if err := manager.AddConfigurations(ctx, workspaceConfigurations...); err != nil {
		return w.Wrapf(err, "cannot add workspace configurations")
	}
	if err := manager.AddConfigurations(ctx, resources.FilterConfigurations(dependencyConfigurations, runtimeContext)...); err != nil {
		return w.Wrapf(err, "cannot add dependency configurations")
	}
	if err := manager.AddConfigurations(ctx, resources.FilterConfigurations(runtimeConfigurations, runtimeContext)...); err != nil {
		return w.Wrapf(err, "cannot add runtime configurations")
	}
	environments, err := manager.All()
	if err != nil {
		return w.Wrapf(err, "cannot get service process environment variables")
	}
	return appendEnvironmentVariablesToFile(ctx, filePath, environments)
}

// outputEnvNetworkMappings is the endpoint set the output environment carries:
// the service's own mappings, plus its dependencies' narrowed by
// resources.ResolveDependencyNetworkMappings to what the service declared and
// its producers permit. The file is a second carrier of the addresses the agent
// itself receives, so it resolves through the same call that
// services.RuntimeInstance.Start makes on the request — otherwise the narrower
// of the two carriers decides what enforcement is worth.
func outputEnvNetworkMappings(
	consumerModule string,
	dependencies []*resources.ServiceDependency,
	own []*basev0.NetworkMapping,
	dependencyMappings []*basev0.NetworkMapping,
) ([]*basev0.NetworkMapping, error) {
	resolved, err := resources.ResolveDependencyNetworkMappings(consumerModule, dependencies, dependencyMappings)
	if err != nil {
		return nil, err
	}
	mappings := make([]*basev0.NetworkMapping, 0, len(own)+len(resolved))
	mappings = append(mappings, own...)
	return append(mappings, resolved...), nil
}

// AppendRuntimeEnvironmentToFile exports the identity and endpoint
// capabilities that agents add during Init and Start. The mappings must include
// the selected service's provided APIs as well as its dependencies: an SDK
// process running outside the service needs both to reproduce that service's
// live runtime environment.
func AppendRuntimeEnvironmentToFile(
	ctx context.Context,
	filePath string,
	identity *basev0.ServiceIdentity,
	runtimeContext *basev0.RuntimeContext,
	fixture string,
	overrides map[string]string,
	endpointMappings []*basev0.NetworkMapping,
) error {
	w := wool.Get(ctx).In("resources.AppendRuntimeEnvironmentToFile", wool.Field("filePath", filePath))
	if identity == nil {
		return w.NewError("service identity is required")
	}
	manager := resources.NewEnvironmentVariableManager()
	manager.SetIdentity(identity)
	manager.SetRuntimeContext(runtimeContext)
	manager.SetFixture(fixture)
	manager.AddOverrides(overrides)
	if err := manager.AddEndpoints(
		ctx,
		endpointMappings,
		resources.NetworkAccessFromRuntimeContext(runtimeContext),
	); err != nil {
		return w.Wrapf(err, "cannot add runtime endpoints")
	}
	environments, err := manager.All()
	if err != nil {
		return w.Wrapf(err, "cannot get runtime environment variables")
	}
	return appendEnvironmentVariablesToFile(ctx, filePath, environments)
}

func appendEnvironmentVariablesToFile(
	ctx context.Context,
	filePath string,
	environments []*resources.EnvironmentVariable,
) error {
	w := wool.Get(ctx).In("resources.AppendToFile", wool.Field("filePath", filePath))
	filePath = strings.TrimSpace(filePath)
	if filePath == "" {
		return w.NewError("output environment file path is required")
	}
	// The output may contain secret service configuration. Refuse symlinks and
	// force owner-only permissions even when appending to a pre-existing file.
	if info, statErr := os.Lstat(filePath); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return w.NewError("output environment file must not be a symbolic link")
		}
	} else if !os.IsNotExist(statErr) {
		return w.Wrapf(statErr, "cannot inspect environment variables file")
	}
	file, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return w.Wrapf(err, "cannot open file")
	}
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		return w.Wrapf(err, "cannot secure environment variables file")
	}

	// Write each environment variable to the file
	for _, env := range environments {
		_, err := fmt.Fprintf(file, "%s=%v\n", env.Key, env.Value)
		if err != nil {
			return w.Wrapf(err, "cannot write to file")
		}
	}
	return nil
}

// runnerInitInputs is everything Runner.Init reads before it calls the agent:
// the dependency views, this service's own configuration, and the addresses it
// proposes to serve.
type runnerInitInputs struct {
	dependenciesEndpoints       []*basev0.Endpoint
	dependenciesNetworkMappings []*basev0.NetworkMapping
	conf                        *basev0.Configuration
	runtimeContext              *basev0.RuntimeContext
	workspaceConfigurations     []*basev0.Configuration
	dependenciesConfigurations  []*basev0.Configuration
	networkMappings             []*basev0.NetworkMapping
}

// gatherInitInputs performs the reads Init needs, each bounded by cfgCtx.
//
// They are gathered in one place because they share one failure mode: a
// configuration read can block on a stalled provider — a dependency that failed
// to export its configuration — and a timeout there is a different diagnosis
// from a real fault, which initReadFailure is what says so.
func (runner *Runner) gatherInitInputs(ctx, cfgCtx context.Context, w *wool.Wool) (*runnerInitInputs, error) {
	out := &runnerInitInputs{}
	var err error
	out.dependenciesEndpoints, err = runner.world.SharedState.GetDependenciesEndpoints(cfgCtx, runner.instance.Service)
	if err != nil {
		return nil, runner.initReadFailure(cfgCtx, w, err, "dependencies endpoints")
	}
	out.dependenciesNetworkMappings, err = runner.world.SharedState.GetDependenciesNetworkMappings(cfgCtx, runner.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get initialized dependency network mappings")
	}
	if runner.testRequest != nil && !runner.serviceRunningForTest && len(out.dependenciesNetworkMappings) > 0 &&
		!slices.Contains(runner.instance.Info.GetContract().GetCapabilities(), contract.RuntimeInitDependencyMappings) {
		return nil, w.NewError("dependency-only tests require agent capability %s to consume accepted addresses at Init", contract.RuntimeInitDependencyMappings)
	}
	out.conf, err = runner.world.ConfigurationManager.GetServiceConfiguration(cfgCtx, runner.instance.Identity)
	if err != nil {
		return nil, runner.initReadFailure(cfgCtx, w, err, "service configuration")
	}
	out.runtimeContext, err = resources.NewRuntimeContext(runner.runtimeContext)
	if err != nil {
		return nil, w.Wrapf(err, "cannot create runtime context: <%s>", runner.runtimeContext)
	}
	out.workspaceConfigurations, err = runner.workspaceConfigurations(cfgCtx, out.dependenciesNetworkMappings, out.runtimeContext)
	if err != nil {
		return nil, runner.initReadFailure(cfgCtx, w, err, "workspace dependencies configurations")
	}
	out.dependenciesConfigurations, err = runner.world.SharedState.GetDependentConfigurationsFor(cfgCtx, runner.instance.Identity)
	if err != nil {
		return nil, runner.initReadFailure(cfgCtx, w, err, "dependencies configurations")
	}
	out.networkMappings, err = runner.world.LocalNetworkManager.GenerateNetworkMappings(ctx, runner.world.Env.Runtime(), runner.world.Workspace, runner.instance.Identity, runner.endpoints, out.runtimeContext)
	if err != nil {
		return nil, w.Wrapf(err, "cannot generate network mappings for service endpoints")
	}
	return out, nil
}

// initReadFailure tells a stalled provider apart from a real fault, in the one
// wording every bounded Init read used to repeat.
func (runner *Runner) initReadFailure(cfgCtx context.Context, w *wool.Wool, err error, what string) error {
	if ContextDeadlineExceeded(err) || ContextDeadlineExceeded(cfgCtx.Err()) {
		w.Warn(fmt.Sprintf("timeout waiting for %s after 30s; check that dependency services are reachable", what))
		return w.Wrapf(err, "init timeout: %s not available within 30s", what)
	}
	return w.Wrapf(err, "cannot get %s", what)
}
