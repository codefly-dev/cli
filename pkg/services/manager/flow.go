package manager

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/providers"
	"github.com/codefly-dev/core/wool"
	multierror "github.com/hashicorp/go-multierror"
)

var currentFlow *Flow

func CurrentFlow() *Flow {
	return currentFlow
}

type Flow struct {
	project *configurations.Project

	// Where we start
	origin *configurations.Service

	// The world
	world *World

	// What we do
	playbook *Playbook
	policy   PlaybookPolicy

	// How we keep track of state
	SharedState *StateManager

	// How we keep track of configurations
	ConfigurationManager *providers.Manager

	hub *Hub

	endpoints       map[string][]*basev0.Endpoint
	networkMappings map[string][]*basev0.NetworkMapping

	loadOnly bool
	initOnly bool

	standAlone  bool
	excludeRoot bool

	native bool

	// convenient
	services map[string]*configurations.Service
}

func MapValues[K comparable, V any](m map[K]V) []V {
	var values []V
	for _, v := range m {
		values = append(values, v)
	}
	return values
}

type World struct {
	Env     *configurations.Environment
	Mode    Mode
	Project *configurations.Project

	// DAG
	Dependencies *architecture.ServiceDependencies

	// Keep track of things
	SharedState *StateManager

	NetworkManager network.Manager

	ConfigurationManager *providers.Manager
}

// NewEmptyFlow will run a single agent
func NewEmptyFlow(ctx context.Context, mode Mode) (*Flow, error) {
	world := &World{
		Mode: mode,
		Env:  configurations.Local(),
	}
	return &Flow{
		world:    world,
		services: make(map[string]*configurations.Service),
	}, nil
}

func NewFlow(ctx context.Context, project *configurations.Project, service *configurations.Service, env *configurations.Environment, mode Mode) (*Flow, error) {
	w := wool.Get(ctx).In("NewFlow")

	services := map[string]*configurations.Service{service.Unique(): service}

	// Get dependency graph
	dependencies, err := architecture.NewServiceDependencies(ctx, project)
	if err != nil {
		return nil, w.Wrap(err)
	}

	configurationManager, err := providers.NewManager(ctx, project)
	if err != nil {
		return nil, w.Wrap(err)
	}
	localReader, err := providers.NewConfigurationLocalReader(ctx, project)
	if err != nil {
		return nil, w.Wrap(err)
	}
	configurationManager.WithLoader(localReader)

	stateManager, err := NewStateManager(ctx, configurationManager, dependencies)
	if err != nil {
		return nil, w.Wrap(err)
	}

	world := &World{
		Env:                  env,
		Mode:                 mode,
		Project:              project,
		SharedState:          stateManager,
		ConfigurationManager: configurationManager,
		Dependencies:         dependencies,
	}

	switch mode {
	case RunMode:
		world.NetworkManager, err = network.NewRuntimeManager(ctx, configurationManager)
	case BuildMode:
		world.NetworkManager, err = network.NewDeployManager(ctx, configurationManager)
	case DeployMode:
		world.NetworkManager, err = network.NewDeployManager(ctx, configurationManager)
	}

	if err != nil {
		return nil, w.Wrap(err)
	}

	flow := &Flow{
		project: project,
		origin:  service,

		services: services,

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

	// Load the configurations
	err := flow.ConfigurationManager.Restrict(ctx, MapValues(flow.services))
	if err != nil {
		return w.Wrap(err)
	}

	err = flow.ConfigurationManager.Load(ctx, flow.world.Env)
	if err != nil {
		return w.Wrap(err)
	}

	allConfs, err := flow.ConfigurationManager.GetConfigurations(ctx)
	if err != nil {
		return w.Wrap(err)
	}

	w.Debug("got configurations",
		wool.Field("confs", configurations.MakeManyConfigurationSummary(allConfs)),
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
				return action.Type == RuntimeLoad && action.Service == flow.origin.Unique()
			})
		}
		if flow.initOnly {
			w.Debug("init only")
			playbook.WithStoppingAfter(func(ctx context.Context, action Action) bool {
				return action.Type == RuntimeInit && action.Service == flow.origin.Unique()
			})
		}
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
			return action.Service == flow.origin.Unique() && action.Type == BuilderBuild
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
			return action.Service == flow.origin.Unique() && action.Type == BuilderSync
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
			return action.Service == flow.origin.Unique() && action.Type == BuilderDeploy
		})

	}
	flow.playbook = playbook

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
		return w.NewError("cannot start nil flow")
	}
	// In stand-alone Mode, we set an ignore policy
	if flow.standAlone {
		flow.playbook.WithIgnore(func(ctx context.Context, action Action) bool {
			return action.Service != flow.origin.Unique()
		})
	}

	err := flow.playbook.Begin(ctx, Action{Type: RuntimeBegin, Service: flow.origin.Unique()})
	if err != nil {
		return w.Wrapf(err, "cannot start playbook")
	}
	return nil
}

func (flow *Flow) Build(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.Build")
	// In stand-alone Mode, we set an ignore policy
	if flow.standAlone {
		flow.playbook.WithIgnore(func(ctx context.Context, action Action) bool {
			return action.Service != flow.origin.Unique()
		})
	}
	err := flow.playbook.Begin(ctx, Action{Type: BuilderBegin, Service: flow.origin.Unique()})
	if err != nil {
		return w.Wrapf(err, "cannot start playbook")
	}
	return nil
}

func (flow *Flow) Sync(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.Sync")
	// In stand-alone Mode, we set an ignore policy
	if flow.standAlone {
		flow.playbook.WithIgnore(func(ctx context.Context, action Action) bool {
			return action.Service != flow.origin.Unique()
		})
	}
	err := flow.playbook.Begin(ctx, Action{Type: BuilderBegin, Service: flow.origin.Unique()})
	if err != nil {
		return w.Wrapf(err, "cannot start playbook")
	}
	return nil
}

func (flow *Flow) Deploy(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.Sync")
	// In stand-alone Mode, we set an ignore policy
	if flow.standAlone {
		flow.playbook.WithIgnore(func(ctx context.Context, action Action) bool {
			return action.Service != flow.origin.Unique()
		})
	}
	err := flow.playbook.Begin(ctx, Action{Type: BuilderBegin, Service: flow.origin.Unique()})
	if err != nil {
		return w.Wrapf(err, "cannot start playbook")
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
		err := manager.Stop(stoppedContext)
		if err != nil {
			w.Debug("got error", wool.ErrField(err))
			res = multierror.Append(res, err)
		}
	}
	return res
}

func (flow *Flow) GetExecutor(ctx context.Context, action Action) (OutputProcessorFunc, error) {
	w := wool.Get(ctx).In("GetExecutor", wool.Field("action", action))
	manager, err := flow.hub.Manager(action.Service)
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

func (flow *Flow) GetAddressForEndpoint(ctx context.Context, application string, service string, endpoint string) (string, error) {
	if flow == nil {
		return "", fmt.Errorf("cannot get address from nil flow")
	}
	if flow.SharedState == nil {
		return "", fmt.Errorf("cannot get addresses from nil state")
	}
	// We get that from the stateManager
	unique := configurations.ServiceUnique(application, service)
	mappings, ok := flow.SharedState.GetNetworkMappingsFromUnique(unique)
	if !ok {
		return "", fmt.Errorf("cannot get network mappings for %s", unique)

	}
	for _, np := range mappings {
		if np.Endpoint.Name == endpoint {
			for _, instance := range np.Instances {
				if instance.Scope == basev0.NetworkScope_Native {
					return instance.Address, nil
				}
			}
		}
	}
	return "", fmt.Errorf("cannot find network mappings for %s", unique)
}

func (flow *Flow) ServiceFromUnique(unique string) *configurations.Service {
	return flow.services[unique]
}

func (flow *Flow) InitManagers(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.InitManagers")
	// Create manager for all services required by this service if not standalone
	var required []string
	if !flow.standAlone {
		order, err := flow.world.Dependencies.OrderTo(ctx, flow.origin.Unique())
		if err != nil {
			return w.Wrapf(err, "cannot order services")
		}
		for _, service := range order {
			required = append(required, service.Unique)
		}
		w.Debug("service dependencies", wool.NameField(flow.origin.Name), wool.Field("dependencies", required))
	}
	if len(required) == 0 {
		cli.Info("Running <%s>", flow.origin.Name)
	} else {
		cli.Info("Running <%s> with these dependent services: %s", flow.origin.Name, strings.Join(required, ", "))
	}
	// We run in the proper order
	slices.Reverse(required)

	var managers []IManager

	for _, unique := range required {
		cli.RegisterLoggingResource(unique)
		info, err := configurations.ParseServiceUnique(unique)
		w.Debug("creating run manager", wool.Field("for", unique))
		if err != nil {
			return w.Wrap(err)
		}
		app, err := flow.project.LoadApplicationFromName(ctx, info.Application)
		if err != nil {
			return w.Wrap(err)
		}
		svc, err := app.LoadServiceFromName(ctx, info.Name)
		if err != nil {
			return w.Wrap(err)
		}
		flow.services[unique] = svc
		// Register source to handle "pretty" logging
		cli.RegisterLoggingResource(unique)
		manager, err := New(ctx, svc, flow.world)
		if err != nil {
			return w.Wrap(err)
		}
		managers = append(managers, manager)
	}

	// Now add the current one
	if !flow.excludeRoot {
		w.Debug("creating run manager", wool.Field("for", flow.origin.Unique()))
		manager, err := New(ctx, flow.origin, flow.world)
		cli.RegisterLoggingResource(flow.origin.Unique())
		if err != nil {
			return w.Wrap(err)
		}
		manager.Runner.WithNative(flow.native)
		managers = append(managers, manager)
	} else {
		// We use a NoOP Manager
		managers = append(managers, &NoOpManager{service: flow.origin})

	}

	flow.hub = &Hub{managers: managers}
	return nil
}

func (flow *Flow) WithGoService(ctx context.Context, args ...string) error {
	w := wool.Get(ctx).In("flow.WithGoService")
	unique := "application/go"
	cur, err := os.Getwd()
	if err != nil {
		return w.Wrapf(err, "can't get current dir")
	}
	agent, err := common.GetAgent(ctx, "codefly.dev/go-single:0.0.18")
	if err != nil {
		return w.Wrapf(err, "cannot get agent")
	}
	svc := &configurations.Service{
		Name:        agent.Name,
		Agent:       agent,
		RuntimeSpec: map[string]any{"run-args": args},
	}
	w.Debug("running with args", wool.Field("args", args))
	svc.WithDir(cur)

	flow.origin = svc
	flow.services[unique] = svc
	var networkManager *network.RuntimeManager
	flow.world.NetworkManager = networkManager
	cli.RegisterLoggingResource(unique)
	return nil
}

func (flow *Flow) CreateManager(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.InitManagers")
	w.Debug("creating run manager", wool.Field("for", flow.origin.Unique()))
	manager, err := New(ctx, flow.origin, flow.world)
	cli.RegisterLoggingResource(flow.origin.Unique())
	if err != nil {
		return w.Wrap(err)
	}
	flow.hub = &Hub{managers: []IManager{manager}}
	return nil
}

func (flow *Flow) Ready(ctx context.Context) bool {
	// We want the origin to have ran
	for _, action := range flow.playbook.Executed() {
		if action.Service == flow.origin.Unique() && action.Type == RuntimeStart {
			return true
		}
	}
	return false
}

func (flow *Flow) WithStandAlone(alone bool) {
	flow.standAlone = alone
}

func (flow *Flow) WithNative(native bool) {
	flow.native = native
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

func (flow *Flow) ActiveProject() *configurations.Project {
	return flow.project
}

func (flow *Flow) Origin() *configurations.Service {
	return flow.origin
}

var _ ExecutorManager = &Flow{}
