package manager

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/providers"
	"github.com/codefly-dev/core/wool"
	"github.com/hashicorp/go-multierror"
)

var currentFlow *Flow

func CurrentFlow() *Flow {
	return currentFlow
}

type Flow struct {
	project *configurations.Project
	origin  *configurations.Service

	world   *World
	actions *ActionManager

	managers []*Manager
	policy   PlaybookPolicy

	endpoints       map[string][]*basev0.Endpoint
	networkMappings map[string][]*basev0.NetworkMapping

	initOnly   bool
	standAlone bool

	// convenient
	services map[string]*configurations.Service
}

type World struct {
	env          *configurations.Environment
	mode         Mode
	dependencies *architecture.ServiceDependencies
	provider     *providers.Provider
}

func NewFlow(ctx context.Context, project *configurations.Project, service *configurations.Service, env *configurations.Environment, mode Mode) (*Flow, error) {
	w := wool.Get(ctx).In("NewFlow")

	services := map[string]*configurations.Service{service.Unique(): service}

	provider, err := providers.New(ctx, project)
	if err != nil {
		return nil, w.Wrap(err)
	}

	// Get dependency graph
	dependencies, err := architecture.NewServiceDependencies(ctx, project)
	if err != nil {
		return nil, w.Wrap(err)
	}

	world := &World{
		env:          env,
		mode:         mode,
		dependencies: dependencies,
		provider:     provider,
	}

	// A Flow really handles creating actions and running them
	// Non-buffered single channel for now to make sure we get the order correct
	// TODO: smart parallelization
	actions := NewActionManager()

	flow := &Flow{
		project: project,
		origin:  service,

		services: services,

		world: world,

		actions: actions,

		endpoints:       make(map[string][]*basev0.Endpoint),
		networkMappings: make(map[string][]*basev0.NetworkMapping),
	}
	currentFlow = flow
	return flow, nil
}

func (flow *Flow) Load(ctx context.Context) error {
	w := wool.Get(ctx).In("NewFlow")

	if flow.standAlone {
		w.Focus("running in stand-alone mode")

	}
	// Create manager for all services required by this service if not standalone
	var required []string
	if !flow.standAlone {
		order, err := flow.world.dependencies.OrderTo(ctx, flow.origin.Unique())
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
		manager, err := New(ctx, svc, flow.world, flow.actions)
		if err != nil {
			return w.Wrap(err)
		}
		managers = append(managers, manager)
	}

	// Now add the current one

	w.Info("creating run manager", wool.Field("for", flow.origin.Unique()))
	manager, err := New(ctx, flow.origin, flow.world, flow.actions)
	cli.RegisterLoggingResource(flow.origin.Unique())
	if err != nil {
		return w.Wrap(err)
	}
	managers = append(managers, manager)
	flow.managers = managers

	var orders []string
	for _, m := range managers {
		orders = append(orders, m.Unique())
	}
	w.Debug("running", wool.Field("order", orders))
	currentFlow = flow
	return nil
}

func (flow *Flow) WithPolicy(policy PlaybookPolicy) *Flow {
	flow.policy = policy
	return flow
}

func (flow *Flow) Start(ctx context.Context) error {
	w := wool.Get(ctx).In("Runner")
	policy, err := NewRuntimeStartPolicy(ctx, flow.world.dependencies, flow)
	if err != nil {
		return w.Wrapf(err, "cannot create policy")
	}
	flow.WithPolicy(policy)
	playbook, err := NewPlaybook(ctx, flow.world.dependencies)
	if err != nil {
		return w.Wrapf(err, "cannot create playbook")
	}
	playbook.WithPolicy(policy)

	// In stand-alone mode, we set an ignore policy
	if flow.standAlone {
		playbook.WithIgnore(func(ctx context.Context, action Action) bool {
			return action.Service != flow.origin.Unique()
		})
	}

	err = playbook.Start(ctx, Action{Type: RuntimeCreate, Service: flow.origin.Unique()})
	if err != nil {
		return w.Wrapf(err, "cannot start playbook")
	}
	return nil
}

func (flow *Flow) Manager(unique string) (*Manager, error) {
	for _, manager := range flow.managers {
		if manager.Unique() == unique {
			return manager, nil
		}
	}
	return nil, fmt.Errorf("no manager found for %s", unique)
}

func (flow *Flow) Stop() error {
	// Don't call on a possibly Done context
	stoppedContext, done := common.NewContext()
	w := wool.Get(stoppedContext).In("Stop")
	defer done()
	var res error
	for _, manager := range flow.managers {
		err := manager.Stop(stoppedContext)
		if err != nil {
			w.Focus("got error", wool.ErrField(err))
			res = multierror.Append(res, err)
		}
	}
	return res
}

func (flow *Flow) GetExecutor(ctx context.Context, action Action) (OutputProcessorFunc, error) {
	w := wool.Get(ctx).In("GetExecutor", wool.Field("action", action))
	manager, err := flow.manager(action.Service)
	if err != nil {
		return nil, w.Wrap(err)
	}
	switch action.Type {
	case RuntimeCreate:
		return func(ctx context.Context) (*OutputProperty, error) {
			return OnInit(), nil
		}, nil
	case RuntimeLoad:
		return manager.Runner.Load, nil
	case RuntimeInit:
		return manager.Runner.Init, nil
	case RuntimeStart:
		return manager.Runner.Start, nil
	default:
		return nil, w.NewError("unknown action type %s", action.Type)
	}
}

func (flow *Flow) WithStandAlone(alone bool) {
	flow.standAlone = alone
}

func (flow *Flow) manager(service string) (*Manager, error) {
	for _, manager := range flow.managers {
		if manager.Unique() == service {
			return manager, nil
		}
	}
	return nil, fmt.Errorf("no manager found for %s", service)
}

var _ ExecutorManager = &Flow{}
