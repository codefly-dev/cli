package manager

import (
	"context"
	"slices"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/deployment"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
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
	sharedState *StateManager

	hub *Hub

	endpoints       map[string][]*basev0.Endpoint
	networkMappings map[string][]*basev0.NetworkMapping

	initOnly   bool
	standAlone bool

	// convenient
	services map[string]*configurations.Service
	ci       bool
}

type World struct {
	Env     *configurations.Environment
	Mode    Mode
	Project *configurations.Project

	// DAG
	Dependencies *architecture.ServiceDependencies

	// Things to know
	Provider *providers.Provider

	// Things to share
	SharedState *StateManager

	Deployer *deployment.LocalManager
}

func NewFlow(ctx context.Context, project *configurations.Project, service *configurations.Service, env *configurations.Environment, mode Mode, ci bool) (*Flow, error) {
	w := wool.Get(ctx).In("NewFlow")

	services := map[string]*configurations.Service{service.Unique(): service}

	deployer, err := deployment.NewLocalManager(ctx)
	if err != nil {
		return nil, w.Wrap(err)
	}

	// Get dependency graph
	dependencies, err := architecture.NewServiceDependencies(ctx, project)
	if err != nil {
		return nil, w.Wrap(err)
	}

	provider, err := providers.New(ctx, project)
	if err != nil {
		return nil, w.Wrap(err)
	}

	stateManager, err := NewStateManager(ctx, provider, dependencies)
	if err != nil {
		return nil, w.Wrap(err)
	}

	world := &World{
		Env:          env,
		Mode:         mode,
		Project:      project,
		Provider:     provider,
		SharedState:  stateManager,
		Dependencies: dependencies,
		Deployer:     deployer,
	}

	flow := &Flow{
		project: project,
		origin:  service,

		ci: ci,

		services: services,

		world: world,

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
	var playbook *Playbook

	err := flow.InitManagers(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot initialize managers")
	}

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
	case BuildMode:
		policy, err := NewBuildPolicy(ctx, flow.hub, flow.world, flow.ci)
		if err != nil {
			return w.Wrapf(err, "cannot create policy")
		}
		flow.WithPolicy(policy)
		playbook, err = NewPlaybook(ctx, flow.world)
		if err != nil {
			return w.Wrapf(err, "cannot create playbook")
		}
		playbook.WithPolicy(policy)
		playbook.WithStopping(func(ctx context.Context, action Action) bool {
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
		playbook.WithStopping(func(ctx context.Context, action Action) bool {
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
		playbook.WithStopping(func(ctx context.Context, action Action) bool {
			return action.Service == flow.origin.Unique() && action.Type == BuilderDeploy
		})

	}
	flow.playbook = playbook

	// Fix the callback
	for _, manager := range flow.hub.managers {
		manager.SetCallback(flow.playbook.Seed)
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
		return manager.Runner.Load, nil
	case RuntimeInit:
		return manager.Runner.Init, nil
	case RuntimeStart:
		return manager.Runner.Start, nil
	case BuilderBegin:
		return func(ctx context.Context) (*OutputProperty, error) {
			return OnInit(), nil
		}, nil
	case BuilderLoad:
		return manager.Builder.Load, nil
	case BuilderInit:
		return manager.Builder.Init, nil
	case BuilderBuild:
		return manager.Builder.Build, nil
	case BuilderSync:
		return manager.Builder.Sync, nil
	case BuilderDeploy:
		return manager.Builder.Deploy, nil

	default:
		return nil, w.NewError("unknown action type %s for executor", action.Type)
	}
}

func (flow *Flow) WithStandAlone(alone bool) {
	flow.standAlone = alone
}

func (flow *Flow) GetAddressesForEndpoint(application string, service string, endpoint string) []string {
	// We get that from the stateManager
	var addresses []string
	unique := configurations.ServiceUnique(application, service)
	for _, np := range flow.sharedState.networkMappings[unique] {
		if np.Endpoint.Name == endpoint {
			addresses = append(addresses, np.Addresses...)
		}
	}
	return addresses
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
		w.Debug("service Dependencies", wool.NameField(flow.origin.Name), wool.Field("Dependencies", required))
	}
	if len(required) == 0 {
		cli.Info("Running <%s>", flow.origin.Name)
	} else {
		cli.Info("Running <%s> with these dependent services: %s", flow.origin.Name, strings.Join(required, ", "))
	}
	// We run in the proper order
	slices.Reverse(required)

	var managers []*Manager

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

	w.Debug("creating run manager", wool.Field("for", flow.origin.Unique()))
	manager, err := New(ctx, flow.origin, flow.world)
	cli.RegisterLoggingResource(flow.origin.Unique())
	if err != nil {
		return w.Wrap(err)
	}
	managers = append(managers, manager)

	flow.hub = &Hub{managers: managers}
	return nil
}

var _ ExecutorManager = &Flow{}
