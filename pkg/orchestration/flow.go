package orchestration

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/deployments"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	multierror "github.com/hashicorp/go-multierror"
)

var currentFlow *Flow

func CurrentFlow() *Flow {
	return currentFlow
}

type Flow struct {
	workspace *resources.Workspace

	// Where we start
	originService *resources.Service
	originModule  *resources.Module

	// The world
	world *World

	// What we do
	playbook *Playbook
	policy   PlaybookPolicy

	// How we keep track of state
	SharedState *StateManager

	// How we keep track of resources
	ConfigurationManager *configurations.Manager

	hub *Hub

	// failures fans in runner crashes from every Manager.Runner.Follow loop
	// so the top-level `codefly run` command can break out of its <-ctx.Done()
	// wait when a child service dies (instead of leaking the parent + plugins).
	// Buffered so a single failure doesn't block the goroutine that posts it.
	failures chan FlowFailure

	endpoints       map[string][]*basev0.Endpoint
	networkMappings map[string][]*basev0.NetworkMapping

	loadOnly bool
	initOnly bool

	standAlone  bool
	excludeRoot bool

	scope string

	runtimeContext string
	fixture        string

	// testRequest carries CLI-provided test filtering/suite/extra-args
	// to the origin runner when the flow is in TestMode. Dependency
	// runners ignore it — they only need to be Started, not tested.
	testRequest *runtimev0.TestRequest

	// Output running configurations
	outputEnvPath string

	// actual services running
	services []*resources.Service
	// except when we do remote
	remoteServices []*Remote

	excludedDependencyServices []string
}

func MapValues[K comparable, V any](m map[K]V) []V {
	var values []V
	for _, v := range m {
		values = append(values, v)
	}
	return values
}

type World struct {
	Env       *resources.Environment
	Mode      Mode
	Workspace *resources.Workspace

	// DAG
	Dependencies *architecture.ServiceDependencies

	// Keep track of things
	SharedState *StateManager

	LocalNetworkManager  *network.RuntimeManager
	RemoteNetworkManager *network.RemoteManager

	ConfigurationManager *configurations.Manager

	RemoteManager deployments.Manager
}

// FlowFailure carries a runner-level death up to the top-level command.
// A failure here means a service started OK but its underlying process
// has since exited (e.g. user binary crashed, agent plugin lost contact).
// It does NOT cover failures during Load/Init/Start — those bubble up
// through the normal flow.Start() return error.
type FlowFailure struct {
	Service string
	Message string
}

func (f FlowFailure) Error() string {
	return fmt.Sprintf("%s: %s", f.Service, f.Message)
}

// NewEmptyFlow will run a single agent
func NewEmptyFlow(ctx context.Context, mode Mode) (*Flow, error) {
	world := &World{
		Mode: mode,
		Env:  resources.LocalEnvironment(),
	}
	return &Flow{
		world: world,
		// Buffer sized to accommodate the largest plausible dependency graph.
		// With 8 we silently dropped failures under concurrent crash cascades
		// (the review caught this); 256 covers realistic sizes while still
		// bounded.
		failures: make(chan FlowFailure, 256),
	}, nil
}

func NewFlow(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, env *resources.Environment, mode Mode) (*Flow, error) {
	w := wool.Get(ctx).In("NewFlow")

	// Get dependency graph
	dependencies, err := architecture.NewServiceDependencies(ctx, workspace)
	if err != nil {
		return nil, w.Wrap(err)
	}

	world := &World{
		Env:          env,
		Mode:         mode,
		Workspace:    workspace,
		Dependencies: dependencies,
	}

	configurationManager, err := configurations.NewManager(ctx, workspace)
	if err != nil {
		return nil, w.Wrap(err)
	}

	localReader, err := configurations.NewConfigurationLocalReader(ctx, workspace)
	if err != nil {
		return nil, w.Wrap(err)
	}
	configurationManager.WithLoader(localReader)

	stateManager, err := NewStateManager(ctx, configurationManager, world.Dependencies)
	if err != nil {
		return nil, w.Wrap(err)
	}

	world.SharedState = stateManager
	world.ConfigurationManager = configurationManager

	world.LocalNetworkManager, err = network.NewRuntimeManager(ctx, configurationManager)
	if err != nil {
		return nil, w.Wrap(err)
	}
	world.RemoteNetworkManager, err = network.NewRemoteManager(ctx, configurationManager)
	if err != nil {
		return nil, w.Wrap(err)
	}

	flow := &Flow{
		workspace:     workspace,
		originService: service,
		originModule:  module,

		world: world,

		SharedState:          stateManager,
		ConfigurationManager: configurationManager,

		// Buffer sized to accommodate the largest plausible dependency graph.
		// With 8 we silently dropped failures under concurrent crash cascades
		// (the review caught this); 256 covers realistic sizes while still
		// bounded.
		failures: make(chan FlowFailure, 256),

		endpoints:       make(map[string][]*basev0.Endpoint),
		networkMappings: make(map[string][]*basev0.NetworkMapping),
	}
	currentFlow = flow
	return flow, nil
}

func (flow *Flow) Load(ctx context.Context) error {
	w := wool.Get(ctx).In("NewFlow")

	if flow.standAlone {
		w.Debug("running in stand-alone Mode")
	}

	// LoadRequired the resources
	var identities []*resources.ServiceIdentity
	for _, service := range flow.services {
		id, err := service.Identity()
		if err != nil {
			return w.Wrap(err)
		}
		identities = append(identities, id)
	}
	err := flow.ConfigurationManager.Restrict(ctx, identities)
	if err != nil {
		return w.Wrap(err)
	}

	err = flow.ConfigurationManager.Load(ctx, flow.world.Env)
	if err != nil {
		return w.Wrap(err)
	}

	w.Debug("got resources",
		wool.Field("dns", flow.ConfigurationManager.DNS()))

	var playbook *Playbook

	switch flow.world.Mode {
	case RunMode:
		policy, err := NewRuntimeStartPolicy(ctx, flow.world.Dependencies, flow)
		if err != nil {
			return w.Wrapf(err, "cannot create policy")
		}
		flow.WithPolicy(policy)
		playbook, err = NewPlaybook(ctx, flow.world)
		if err != nil {
			return w.Wrapf(err, "cannot create playbook")
		}
		playbook.WithPolicy(policy)
		if flow.loadOnly {
			w.Debug("load only")
			playbook.WithStoppingAfter(func(ctx context.Context, action Action) bool {
				return action.Type == RuntimeLoad && action.Service == resources.WithUnique(flow.originService).Unique()
			})
		}
		if flow.initOnly {
			w.Debug("init only")
			playbook.WithStoppingAfter(func(ctx context.Context, action Action) bool {
				return action.Type == RuntimeInit && action.Service == resources.WithUnique(flow.originService).Unique()
			})
		}
	case TestMode:
		policy, err := NewRuntimeTestPolicy(ctx, flow.world.Dependencies, flow)
		if err != nil {
			return w.Wrapf(err, "cannot create policy")
		}
		flow.WithPolicy(policy)
		playbook, err = NewPlaybook(ctx, flow.world)
		if err != nil {
			return w.Wrapf(err, "cannot create playbook")
		}
		playbook.WithPolicy(policy)
		if flow.loadOnly {
			w.Debug("load only")
			playbook.WithStoppingAfter(func(ctx context.Context, action Action) bool {
				return action.Type == RuntimeLoad && action.Service == resources.WithUnique(flow.originService).Unique()
			})
		}
		if flow.initOnly {
			w.Debug("init only")
			playbook.WithStoppingAfter(func(ctx context.Context, action Action) bool {
				return action.Type == RuntimeInit && action.Service == resources.WithUnique(flow.originService).Unique()
			})
		}
		playbook.WithStoppingAfter(func(ctx context.Context, action Action) bool {
			return action.Service == resources.WithUnique(flow.originService).Unique() && action.Type == RuntimeTest
		})

	case BuildMode:
		policy, err := NewBuildPolicy(ctx, flow.hub, flow.world)
		if err != nil {
			return w.Wrapf(err, "cannot create policy")
		}
		flow.WithPolicy(policy)
		playbook, err = NewPlaybook(ctx, flow.world)
		if err != nil {
			return w.Wrapf(err, "cannot create playbook")
		}
		playbook.WithPolicy(policy)
		playbook.WithStoppingAfter(func(ctx context.Context, action Action) bool {
			return action.Service == resources.WithUnique(flow.originService).Unique() && action.Type == BuilderBuild
		})
	case SyncMode:
		policy, err := NewSyncPolicy(ctx, flow.world.Dependencies, flow)
		if err != nil {
			return w.Wrapf(err, "cannot create policy")
		}
		flow.WithPolicy(policy)
		playbook, err = NewPlaybook(ctx, flow.world)
		if err != nil {
			return w.Wrapf(err, "cannot create playbook")
		}
		playbook.WithPolicy(policy)
		playbook.WithStoppingAfter(func(ctx context.Context, action Action) bool {
			return action.Service == resources.WithUnique(flow.originService).Unique() && action.Type == BuilderSync
		})
	case DeployMode:
		policy, err := NewDeployPolicy(ctx, flow.world.Dependencies, flow)
		if err != nil {
			return w.Wrapf(err, "cannot create policy")
		}
		flow.WithPolicy(policy)
		playbook, err = NewPlaybook(ctx, flow.world)
		if err != nil {
			return w.Wrapf(err, "cannot create playbook")
		}
		playbook.WithPolicy(policy)
		playbook.WithStoppingAfter(func(ctx context.Context, action Action) bool {
			return action.Service == resources.WithUnique(flow.originService).Unique() && action.Type == BuilderDeploy
		})

	}
	flow.playbook = playbook

	// Fix the callback
	for _, manager := range flow.hub.managers {
		manager.DoSetCallback(flow.playbook.Seed)
		// Wire runner-level failures (post-start crashes) into the flow's
		// failures channel so `codefly run` can break out of <-ctx.Done().
		manager.DoSetFailureSink(flow.reportFailure)
	}

	currentFlow = flow
	return nil
}

func (flow *Flow) WithPolicy(policy PlaybookPolicy) *Flow {
	flow.policy = policy
	return flow
}

func (flow *Flow) Start(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.Begin")
	if flow == nil {
		return w.NewError("cannot execute nil flow")
	}
	// In stand-alone Mode, we set an ignore policy
	if flow.standAlone {
		flow.playbook.WithIgnore(func(ctx context.Context, action Action) bool {
			return action.Service != resources.WithUnique(flow.originService).Unique()
		})
	}

	err := flow.playbook.Begin(ctx, Action{Type: RuntimeBegin, Service: resources.WithUnique(flow.originService).Unique()})
	if err != nil {
		return w.Wrapf(err, "cannot execute start playbook")
	}
	return nil
}

func (flow *Flow) Test(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.Begin")
	if flow == nil {
		return w.NewError("cannot execute nil flow")
	}
	// In stand-alone Mode, we set an ignore policy
	if flow.standAlone {
		flow.playbook.WithIgnore(func(ctx context.Context, action Action) bool {
			return action.Service != resources.WithUnique(flow.originService).Unique()
		})
	}

	err := flow.playbook.Begin(ctx, Action{Type: RuntimeBegin, Service: resources.WithUnique(flow.originService).Unique()})
	if err != nil {
		return w.Wrapf(err, "cannot execute test playbook")
	}
	return nil
}

func (flow *Flow) Build(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.Build")
	// In stand-alone Mode, we set an ignore policy
	if flow.standAlone {
		flow.playbook.WithIgnore(func(ctx context.Context, action Action) bool {
			return action.Service != resources.WithUnique(flow.originService).Unique()
		})
	}
	err := flow.playbook.Begin(ctx, Action{Type: BuilderBegin, Service: resources.WithUnique(flow.originService).Unique()})
	if err != nil {
		return w.Wrapf(err, "cannot execute build playbook")
	}
	return nil
}

func (flow *Flow) Sync(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.Sync")
	// In stand-alone Mode, we set an ignore policy
	if flow.standAlone {
		flow.playbook.WithIgnore(func(ctx context.Context, action Action) bool {
			return action.Service != resources.WithUnique(flow.originService).Unique()
		})
	}
	err := flow.playbook.Begin(ctx, Action{Type: BuilderBegin, Service: resources.WithUnique(flow.originService).Unique()})
	if err != nil {
		return w.Wrapf(err, "cannot execute sync playbook")
	}
	return nil
}

func (flow *Flow) Deploy(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.Deploy")
	// In stand-alone Mode, we set an ignore policy
	if flow.standAlone {
		flow.playbook.WithIgnore(func(ctx context.Context, action Action) bool {
			return action.Service != resources.WithUnique(flow.originService).Unique()
		})
	}
	err := flow.playbook.Begin(ctx, Action{Type: BuilderBegin, Service: resources.WithUnique(flow.originService).Unique()})
	if err != nil {
		return w.Wrapf(err, "cannot execute deploy playbook")
	}
	return nil

}

// Failures returns a read-only channel of runner-level failures.
// Top-level commands (`codefly run service ...`) should select on this
// alongside ctx.Done() so an orphaned plugin tree triggers shutdown.
func (flow *Flow) Failures() <-chan FlowFailure {
	if flow == nil {
		return nil
	}
	return flow.failures
}

// reportFailure is called by Manager/Runner when a follow loop detects
// that a started service died. Non-blocking: drops on a full channel
// because by then the receiver is already shutting things down.
func (flow *Flow) reportFailure(unique, msg string) {
	if flow == nil || flow.failures == nil {
		return
	}
	select {
	case flow.failures <- FlowFailure{Service: unique, Message: msg}:
	default:
		// Buffer saturated. This should never happen in practice (256 is
		// well above any realistic concurrent failure count) but if it
		// does, surface it so the operator knows a failure was dropped
		// rather than silently swallowing it.
		wool.Get(context.Background()).In("Flow.reportFailure").Warn(
			"failure channel full — dropping",
			wool.Field("service", unique),
			wool.Field("message", msg))
	}
}

// ManagedServices returns the origin service name and the dependency service
// names this flow manages. flow.Stop() tears DOWN all of them — origin AND
// dependencies (neo4j, postgres, …) alike — so the runner UI can name exactly
// what is going away instead of a vague "stopping service". Nothing the flow
// started "stays alive" on stop; only nix-run service DATA persists on disk.
func (flow *Flow) ManagedServices() (origin string, dependencies []string) {
	if flow == nil {
		return "", nil
	}
	if flow.originService != nil {
		origin = flow.originService.Name
	}
	for _, s := range flow.services {
		if s == nil || s.Name == origin {
			continue
		}
		dependencies = append(dependencies, s.Name)
	}
	return origin, dependencies
}

func (flow *Flow) Stop() error {
	if flow == nil {
		return nil
	}
	// Don't call on a possibly Done context
	stoppedContext, done := common.NewContext()
	w := wool.Get(stoppedContext).In("StopIfNeeded")
	defer done()
	// Clear any stale pause state — if a paused action is still sitting
	// in the PauseManager, the spinner keeps spinning even as Stop tears
	// everything down. Force-clear so the UI reflects reality.
	if flow.playbook != nil && flow.playbook.pause != nil {
		flow.playbook.pause.Clear()
	}
	// Fan out stops in parallel — sequential iteration was wasting wall
	// time (10s timeout × N managers) while the goroutines were mostly
	// idle waiting on their respective agents. Reverse order is preserved
	// by walking the slice backwards before the Add, so Destroy targets
	// newest-started-first which matches dependency rules.
	var res error
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := len(flow.hub.managers) - 1; i >= 0; i-- {
		mgr := flow.hub.managers[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := mgr.RunnerDoStop(stoppedContext)
			if err != nil {
				w.Debug("got error", wool.ErrField(err))
				mu.Lock()
				res = multierror.Append(res, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return res
}

func (flow *Flow) Shutdown() error {
	if flow == nil {
		return nil
	}
	// Don't call on a possibly Done context
	stoppedContext, done := common.NewContext()
	w := wool.Get(stoppedContext).In("StopIfNeeded")
	defer done()
	if flow.playbook != nil && flow.playbook.pause != nil {
		flow.playbook.pause.Clear()
	}
	var res error
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := len(flow.hub.managers) - 1; i >= 0; i-- {
		mgr := flow.hub.managers[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := mgr.RunnerDoDestroy(stoppedContext)
			if err != nil {
				w.Debug("got error", wool.ErrField(err))
				mu.Lock()
				res = multierror.Append(res, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return res

}

func (flow *Flow) GetExecutor(ctx context.Context, action Action) (OutputProcessorFunc, error) {
	w := wool.Get(ctx).In("GetExecutor", wool.Field("action", action))
	manager, err := flow.hub.NewManager(action.Service)
	if err != nil {
		return nil, w.Wrap(err)
	}
	if action.Failed {
		return func(ctx context.Context) (*OutputProperty, error) {
			return Pause(), nil
		}, nil
	}
	switch action.Type {
	case RuntimeBegin:
		return func(ctx context.Context) (*OutputProperty, error) {
			return OnInit(), nil
		}, nil
	case RuntimeLoad:
		return manager.RunnerDoLoad, nil
	case RuntimeInit:
		return manager.RunnerDoInit, nil
	case RuntimeStart:
		return manager.RunnerDoStart, nil
	case RuntimeTest:
		return manager.RunnerDoTest, nil
	case BuilderBegin:
		return func(ctx context.Context) (*OutputProperty, error) {
			return OnInit(), nil
		}, nil
	case BuilderLoad:
		return manager.BuilderDoLoad, nil
	case BuilderInit:
		return manager.BuilderDoInit, nil
	case BuilderBuild:
		return manager.BuilderDoBuild, nil
	case BuilderSync:
		return manager.BuilderDoSync, nil
	case BuilderDeploy:
		return manager.BuilderDoDeploy, nil

	default:
		return nil, w.NewError("unknown action type %s for executor", action.Type)
	}
}

func (flow *Flow) GetDependenciesNetworkMappingsFor(ctx context.Context, service *resources.Service) ([]*basev0.NetworkMapping, error) {
	if flow == nil {
		return nil, nil
	}
	if flow.SharedState == nil {
		return nil, nil
	}
	return flow.SharedState.GetDependenciesNetworkMappings(ctx, service)
}

func (flow *Flow) GetAddressForEndpoint(ctx context.Context, module string, service string, endpoint string) (string, error) {
	if flow == nil {
		return "", fmt.Errorf("cannot get address from nil flow")
	}
	if flow.SharedState == nil {
		return "", fmt.Errorf("cannot get addresses from nil state")
	}
	// We get that from the stateManager
	unique := resources.ServiceUnique(module, service)
	mappings, ok := flow.SharedState.GetNetworkMappingsFromUnique(unique)
	if !ok {
		return "", fmt.Errorf("cannot get network mappings for %s", unique)

	}
	for _, np := range mappings {
		if np.Endpoint.Name == endpoint {
			for _, instance := range np.Instances {
				if instance.Access.Kind == resources.NetworkAccessPublic {
					return instance.Address, nil
				}
			}
		}
	}
	return "", fmt.Errorf("cannot find network mappings for %s", unique)
}

func (flow *Flow) ServiceFromUnique(unique string) (*resources.Service, error) {
	return flow.world.Dependencies.ServiceFromUnique(unique)
}

func (flow *Flow) InitManagers(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.InitManagers")
	remotes := make(map[string]*Remote)
	var dependencyOptions []architecture.DependencyOption
	if len(flow.remoteServices) > 0 {
		var cutoffs []string
		for _, remote := range flow.remoteServices {
			remotes[remote.Unique()] = remote
			cutoffs = append(cutoffs, remote.Unique())
		}
		dependencyOptions = append(dependencyOptions, architecture.SkipDependencyFor(cutoffs...))
	}
	if len(flow.excludedDependencyServices) > 0 {
		dependencyOptions = append(dependencyOptions, architecture.ExcludeServices(flow.excludedDependencyServices...))
	}
	if len(dependencyOptions) > 0 {
		dep, err := architecture.NewServiceDependencies(ctx, flow.workspace, dependencyOptions...)
		if err != nil {
			return w.Wrap(err)
		}
		flow.world.Dependencies = dep
		if flow.SharedState != nil {
			flow.SharedState.SetDependencies(dep)
		}
	}

	// Create manager for all service required by this service if not standalone
	var required []string
	if !flow.standAlone {
		order, err := flow.world.Dependencies.OrderTo(ctx, resources.WithUnique(flow.originService).Unique())
		if err != nil {
			return w.Wrapf(err, "cannot order services")
		}
		for _, service := range order {
			required = append(required, service.Unique)
		}
		w.Debug("service dependencies", wool.NameField(flow.originService.Name), wool.Field("dependencies", required))
	}
	if len(required) == 0 {
		cli.Info("Handling <%s>", flow.originService.Name)
	} else {
		cli.Info("Handling <%s> with these dependent services: %s", flow.originService.Name, strings.Join(required, ", "))
	}
	// We run in the proper order
	slices.Reverse(required)

	var managers []IManager

	for _, unique := range required {
		cli.RegisterLoggingResource(unique)
		// Register source to handle "pretty" logging

		info, err := resources.ParseServiceWithOptionalModule(unique)
		w.Debug("creating run manager", wool.Field("for", unique))
		if err != nil {
			return w.Wrap(err)
		}

		mod, err := flow.workspace.LoadModuleFromName(ctx, info.Module)
		if err != nil {
			return w.Wrap(err)
		}

		svc, err := mod.LoadServiceFromName(ctx, info.Name)
		if err != nil {
			return w.Wrap(err)
		}

		flow.services = append(flow.services, svc)

		manager, err := New(ctx, mod, svc, flow.world)
		if err != nil {
			return w.Wrap(err)
		}

		manager.Runner.WithRuntimeContext(flow.runtimeContext)
		manager.Runner.WithFixture(flow.fixture)
		manager.Runner.WithOutputEnv(flow.outputEnvPath)
		if remote, ok := remotes[unique]; ok {
			manager.Runner.WithRemote(remote.Environment)
		}
		managers = append(managers, manager)
	}

	// Now add the current one
	if !flow.excludeRoot {
		w.Debug("creating run manager", wool.Field("for", resources.WithUnique(flow.originService).Unique()))
		manager, err := New(ctx, flow.originModule, flow.originService, flow.world)
		cli.RegisterLoggingResource(resources.WithUnique(flow.originService).Unique())
		if err != nil {
			return w.Wrap(err)
		}
		flow.services = append(flow.services, flow.originService)
		manager.Runner.WithRuntimeContext(flow.runtimeContext)
		manager.Runner.WithFixture(flow.fixture)
		manager.Runner.WithOutputEnv(flow.outputEnvPath)
		if flow.testRequest != nil {
			manager.Runner.WithTestRequest(flow.testRequest)
		}
		if remote, ok := remotes[resources.WithUnique(flow.originService).Unique()]; ok {
			manager.Runner.WithRemote(remote.Environment)
		}
		managers = append(managers, manager)
	} else {
		// We use a NoOP NewManager
		managers = append(managers, &NoOpManager{service: flow.originService})

	}

	flow.hub = &Hub{managers: managers}
	return nil
}

func (flow *Flow) CreateManager(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.InitManagers")
	w.Debug("creating run manager", wool.Field("for", resources.WithUnique(flow.originService).Unique()))
	manager, err := New(ctx, flow.originModule, flow.originService, flow.world)
	cli.RegisterLoggingResource(resources.WithUnique(flow.originService).Unique())
	if err != nil {
		return w.Wrap(err)
	}
	flow.hub = &Hub{managers: []IManager{manager}}
	return nil
}

func (flow *Flow) Ready(ctx context.Context) bool {
	if flow == nil || flow.playbook == nil {
		return false
	}
	origin := resources.WithUnique(flow.originService).Unique()
	executed := flow.playbook.Executed()
	if !flow.excludeRoot {
		return runtimeStarted(executed, origin)
	}

	return flow.dependenciesReady(ctx, origin)
}

func (flow *Flow) dependenciesReady(ctx context.Context, origin string) bool {
	if flow.world == nil || flow.world.Dependencies == nil {
		return true
	}
	required, err := flow.world.Dependencies.DirectRequires(ctx, origin)
	if err != nil {
		wool.Get(ctx).In("flow.Ready").Debug("cannot resolve dependencies", wool.ErrField(err))
		return false
	}
	if len(required) == 0 {
		return true
	}
	if flow.SharedState == nil {
		return false
	}
	for _, service := range required {
		svc, err := flow.world.Dependencies.ServiceFromUnique(service.Unique)
		if err != nil {
			wool.Get(ctx).In("flow.Ready").Debug("cannot resolve dependency service", wool.ErrField(err))
			return false
		}
		if len(svc.Endpoints) == 0 {
			continue
		}
		mappings, ok := flow.SharedState.GetNetworkMappingsFromUnique(service.Unique)
		if !ok || len(mappings) == 0 {
			return false
		}
		if !networkMappingsReachable(mappings) {
			return false
		}
	}
	return true
}

func networkMappingsReachable(mappings []*basev0.NetworkMapping) bool {
	byName := make(map[string]*basev0.NetworkMapping)
	for _, mapping := range mappings {
		if mapping.Endpoint != nil {
			byName[mapping.Endpoint.Name] = mapping
		}
	}
	if bolt, hasBolt := byName["bolt"]; hasBolt {
		if httpMapping, hasHTTP := byName["http"]; hasHTTP {
			return networkMappingTCPReachable(bolt) && networkMappingHTTPReachable(httpMapping)
		}
	}

	anyReachable := false
	for _, mapping := range mappings {
		if networkMappingTCPReachable(mapping) {
			anyReachable = true
		}
	}
	return anyReachable
}

func networkMappingTCPReachable(mapping *basev0.NetworkMapping) bool {
	instance := reachableNetworkInstance(mapping.Instances)
	if instance == nil {
		return false
	}
	address := networkInstanceDialAddress(instance)
	if address == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func networkMappingHTTPReachable(mapping *basev0.NetworkMapping) bool {
	instance := reachableNetworkInstance(mapping.Instances)
	if instance == nil {
		return false
	}
	address := networkInstanceDialAddress(instance)
	if address == "" {
		return false
	}
	if !strings.HasPrefix(address, "http://") && !strings.HasPrefix(address, "https://") {
		address = "http://" + address
	}
	client := &http.Client{Timeout: 300 * time.Millisecond}
	resp, err := client.Get(address)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

func reachableNetworkInstance(instances []*basev0.NetworkInstance) *basev0.NetworkInstance {
	for _, instance := range instances {
		if instance.GetAccess().GetKind() == resources.NetworkAccessNative {
			return instance
		}
	}
	for _, instance := range instances {
		if instance.GetAccess().GetKind() == resources.NetworkAccessPublic {
			return instance
		}
	}
	if len(instances) == 0 {
		return nil
	}
	return instances[0]
}

func networkInstanceDialAddress(instance *basev0.NetworkInstance) string {
	if instance.GetHost() != "" {
		return instance.GetHost()
	}
	if instance.GetHostname() != "" && instance.GetPort() != 0 {
		return net.JoinHostPort(instance.GetHostname(), strconv.Itoa(int(instance.GetPort())))
	}
	address := instance.GetAddress()
	u, err := url.Parse(address)
	if err == nil && u.Host != "" {
		return u.Host
	}
	return address
}

func runtimeStarted(actions []Action, unique string) bool {
	for _, action := range actions {
		if action.Service == unique && action.Type == RuntimeStart && !action.Failed {
			return true
		}
	}
	return false
}

func (flow *Flow) WithDeploymentManager(manager deployments.Manager) {
	flow.world.RemoteManager = manager
}

func (flow *Flow) WithStandAlone(alone bool) {
	flow.standAlone = alone
}

func (flow *Flow) WithRuntimeContext(runtimeContext string) {
	flow.runtimeContext = runtimeContext
}

func (flow *Flow) WithFixture(fixture string) {
	flow.fixture = fixture
}

func (flow *Flow) WithExcludeRoot(excludeRoot bool) {
	flow.excludeRoot = excludeRoot
}

func (flow *Flow) WithInitOnly(only bool) {
	flow.initOnly = only
}

func (flow *Flow) Executed() []Action {
	return flow.playbook.Executed()
}

func (flow *Flow) WithLoadOnly(only bool) {
	flow.loadOnly = only

}

// WithTestRequest sets the TestRequest forwarded to the origin runner's
// Test RPC. Only relevant in TestMode; ignored otherwise.
func (flow *Flow) WithTestRequest(req *runtimev0.TestRequest) {
	flow.testRequest = req
}

func (flow *Flow) ActiveWorkspace() *resources.Workspace {
	return flow.workspace
}

func (flow *Flow) Origin() *resources.Service {
	return flow.originService
}

func (flow *Flow) WithOutputEnv(envPath string) {
	// Delete the file first
	if exists, err := shared.FileExists(context.Background(), envPath); err == nil && exists {
		err := shared.DeleteFile(context.Background(), envPath)
		if err != nil {
			cli.Error("cannot delete file %s: %s", envPath, err)
		}
	}
	flow.outputEnvPath = envPath
}

type Remote struct {
	*resources.ServiceWithModule
	*resources.Environment
}

func (flow *Flow) WithRemotes(services []*Remote) {
	flow.remoteServices = services
}

func (flow *Flow) WithExcludedDependencies(services []string) {
	flow.excludedDependencyServices = services
}

var _ ExecutorManager = &Flow{}
