package orchestration

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/deployments"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
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

	endpoints       map[string][]*basev0.Endpoint
	networkMappings map[string][]*basev0.NetworkMapping

	loadOnly bool
	initOnly bool

	standAlone  bool
	excludeRoot bool

	scope string

	runtimeContext string
	fixture        string

	// Output running configurations
	outputEnvPath string

	// actual services running
	services []*resources.Service
	// except when we do remote
	remoteServices []*Remote

	// Stop after a specific action
	stopAfter *Action
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

// NewEmptyFlow will run a single agent
func NewEmptyFlow(ctx context.Context, mode Mode) (*Flow, error) {
	world := &World{
		Mode: mode,
		Env:  resources.LocalEnvironment(),
	}
	return &Flow{
		world: world,
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

	if flow.stopAfter != nil {
		flow.playbook.WithStoppingAfter(func(ctx context.Context, action Action) bool {
			return action.Type == flow.stopAfter.Type && action.Service == flow.stopAfter.Service
		})
	}

	// Fix the callback
	for _, manager := range flow.hub.managers {
		manager.DoSetCallback(flow.playbook.Seed)
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
	w := wool.Get(ctx).In("flow.Sync")
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

func (flow *Flow) Stop() error {
	if flow == nil {
		return nil
	}
	// Don't call on a possibly Done context
	stoppedContext, done := common.NewContext()
	w := wool.Get(stoppedContext).In("StopIfNeeded")
	defer done()
	var res error
	for _, manager := range flow.hub.managers {
		_, err := manager.RunnerDoStop(stoppedContext)
		if err != nil {
			w.Debug("got error", wool.ErrField(err))
			res = multierror.Append(res, err)
		}
	}
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
	var res error
	for _, manager := range flow.hub.managers {
		_, err := manager.RunnerDoDestroy(stoppedContext)
		if err != nil {
			w.Debug("got error", wool.ErrField(err))
			res = multierror.Append(res, err)
		}
	}
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
	if len(flow.remoteServices) > 0 {
		var cutoffs []string
		for _, remote := range flow.remoteServices {
			remotes[remote.Unique()] = remote
			cutoffs = append(cutoffs, remote.Unique())
		}
		dep, err := architecture.NewServiceDependencies(ctx, flow.workspace, architecture.SkipDependencyFor(cutoffs...))
		if err != nil {
			return w.Wrap(err)
		}
		flow.world.Dependencies = dep
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
	if flow == nil {
		return false
	}
	// We want the originService to have ran
	for _, action := range flow.playbook.Executed() {
		if action.Service == resources.WithUnique(flow.originService).Unique() && action.Type == RuntimeStart {
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

var _ ExecutorManager = &Flow{}

func ParseStopAfter(stopAfter string) (Action, error) {
	parts := strings.Split(stopAfter, ":")
	if len(parts) != 2 {
		return Action{}, fmt.Errorf("invalid stop-after format")
	}
	actionType := ActionType(parts[0])
	service, err := resources.ParseServiceWithOptionalModule(parts[1])
	if err != nil {
		return Action{}, fmt.Errorf("invalid stop-after format")
	}
	return Action{Type: actionType, Service: service.Unique()}, nil
}

func (flow *Flow) WithStopAfter(stopAfter string) {
	stoppingAfter, err := ParseStopAfter(stopAfter)
	if err != nil {
		cli.Error("invalid stop-after format: %s", err)
		return
	}
	fmt.Println("stoppingAfter", stoppingAfter)
	flow.stopAfter = &stoppingAfter
}
