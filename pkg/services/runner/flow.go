package runner

import (
	"context"

	"github.com/codefly-dev/core/agents/network"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/configurations"
	basev1 "github.com/codefly-dev/core/generated/go/base/v1"
	runtimev1 "github.com/codefly-dev/core/generated/go/services/runtime/v1"
	"github.com/codefly-dev/core/runners"
	"github.com/codefly-dev/core/wool"
)

type FlowRunner struct {
	managers        []*RunManager
	dependencies    *architecture.Graph
	endpoints       map[string][]*basev1.Endpoint
	networkMappings map[string][]*runtimev1.NetworkMapping
	actions         chan runners.Action
}

func NewFlow(ctx context.Context, project *configurations.Project, service *configurations.Service) (*FlowRunner, error) {
	w := wool.Get(ctx).In("NewFlow")
	// Get dependency graph
	g, err := architecture.LoadServiceGraph(ctx, project)
	if err != nil {
		return nil, w.Wrap(err)
	}
	// Create manager for all services required by this service
	uniques := g.TopologicalSortFrom(service.Unique())
	w.Debug("service dependencies", wool.NameField(service.Name), wool.Field("dependencies", uniques))

	var managers []*RunManager

	for _, unique := range uniques {
		info, err := configurations.ParseServiceUnique(unique)
		w.Info("creating run manager", wool.Field("for", unique))
		if err != nil {
			return nil, w.Wrap(err)
		}
		app, err := project.LoadApplicationFromName(ctx, info.Application)
		if err != nil {
			return nil, w.Wrap(err)
		}
		svc, err := app.LoadServiceFromName(ctx, info.Name)
		if err != nil {
			return nil, w.Wrap(err)
		}
		manager, err := New(ctx, svc)
		if err != nil {
			return nil, w.Wrap(err)
		}
		managers = append(managers, manager)
	}

	// Now add the current one

	w.Info("creating run manager", wool.Field("for", service.Unique()))
	manager, err := New(ctx, service)
	if err != nil {
		return nil, w.Wrap(err)
	}
	managers = append(managers, manager)
	return &FlowRunner{
		managers:        managers,
		endpoints:       make(map[string][]*basev1.Endpoint),
		networkMappings: make(map[string][]*runtimev1.NetworkMapping),
		dependencies:    g,
		actions:         make(chan runners.Action, 1),
	}, nil
}

// Start for the FloRunner works exactly like the RunManager except we don't run all managers, only the required ones
// TODO: Fix logic with unit tests
func (flow *FlowRunner) Start(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.Start")
	w.Debug("sending init")
	flow.actions <- runners.Action{Type: runners.Init}
	for {
		select {
		case action := <-flow.actions:
			switch action.Type {
			case runners.Noop:
				w.Debug("received noop")
			case runners.Init:
				w.Debug("received init")
				err := flow.Init(ctx)
				if err != nil {
					w.Debug("cannot initialize service")
					flow.actions <- runners.Action{Type: runners.Noop}
				} else {
					w.Debug("sending start")
					flow.actions <- runners.Action{Type: runners.Start}
				}
			case runners.Start:
				w.Debug("received start")
				err := flow.Run(ctx)
				if err != nil {
					w.Debug("cannot start service")
				}
				flow.actions <- runners.Action{Type: runners.Noop}
			case runners.Restart:
				w.Debug("restarting")
				err := flow.Stop()
				if err != nil {
					return w.Wrapf(err, "can't stop")
				}
				// Create new runner
				err = flow.Load(ctx)
				if err != nil {
					return w.Wrapf(err, "can't create new runner")
				}
				flow.actions <- runners.Action{Type: runners.Init}
			default:
				return w.NewError("unknown action type")
			}
		case <-ctx.Done():
			return flow.Stop()
		}
	}
}

// Load loads the service
// Request: No dependencies
// Response: Endpoints
func (flow *FlowRunner) Load(ctx context.Context) error {
	w := wool.Get(ctx).In("FlowRunner.Load")
	for _, manager := range flow.managers {
		err := manager.Load(ctx)
		if err != nil {
			return w.Wrapf(err, "cannot load service <%s>", manager.runner.instance.Service.Unique())
		}
		flow.endpoints[manager.Unique()] = manager.loaded.Endpoints
	}
	return nil
}

// Init runs all init
// Init Request: Endpoint group
func (flow *FlowRunner) Init(ctx context.Context) error {
	w := wool.Get(ctx).In("FlowRunner.Init")
	for _, manager := range flow.managers {
		dependenciesEndpoints, err := flow.DependenciesEndpoints(manager.Unique())
		if err != nil {
			return w.Wrapf(err, "cannot get endpoint group for <%s>", manager.Unique())
		}
		err = manager.WithEndpointDependencies(dependenciesEndpoints).Init(ctx)
		if err != nil {
			return w.Wrapf(err, "cannot init service <%s>", manager.Unique())
		}
		w.Debug("init", wool.Field("for", manager.Unique()), wool.NullableField("endpoint dependencies", configurations.MakeEndpointSummary(dependenciesEndpoints)))
		flow.networkMappings[manager.Unique()] = manager.init.NetworkMappings
	}
	return nil
}

func (flow *FlowRunner) Run(ctx context.Context) error {
	w := wool.Get(ctx).In("FlowRunner.Run")
	for _, manager := range flow.managers {
		dependenciesNetworkMappings, err := flow.DependenciesNetworkMappings(manager.Unique())
		if err != nil {
			return w.Wrapf(err, "cannot get network mappings for <%s>", manager.Unique())
		}
		err = manager.WithNetworkMappings(dependenciesNetworkMappings).Run(ctx)
		if err != nil {
			return w.Wrapf(err, "cannot run service <%s>", manager.Unique())
		}
		w.Debug("run", wool.Field("for", manager.Unique()), wool.NullableField("network mappings", network.MakeNetworkMappingSummary(dependenciesNetworkMappings)))
	}
	return nil

}

func (flow *FlowRunner) DependenciesEndpoints(unique string) ([]*basev1.Endpoint, error) {
	w := wool.Get(context.Background()).In("FlowRunner.DependenciesEndpoints")
	// Gather all endpoints from the direct dependencies
	dependencies := flow.dependencies.Antecedents(unique)
	var endpoints []*basev1.Endpoint
	for _, dependency := range dependencies {
		endpoints = append(endpoints, flow.endpoints[dependency]...)
	}
	w.Debug("getting dependencies endpoints", wool.SliceCountField(endpoints), wool.Field("for", unique), wool.NullableField("dependencies", dependencies))
	return endpoints, nil
}

func (flow *FlowRunner) DependenciesNetworkMappings(unique string) ([]*runtimev1.NetworkMapping, error) {
	w := wool.Get(context.Background()).In("FlowRunner.DependenciesNetworkMappings")
	// Gather all mappings from the direct dependencies
	dependencies := flow.dependencies.Antecedents(unique)
	var mappings []*runtimev1.NetworkMapping
	for _, dependency := range dependencies {
		mappingsForDependency := flow.networkMappings[dependency]
		mappings = append(mappings, mappingsForDependency...)
	}
	w.Debug("getting dependencies network mappings", wool.SliceCountField(mappings), wool.Field("for", unique), wool.NullableField("dependencies", dependencies))
	return mappings, nil
}

func (flow *FlowRunner) Stop() error {
	for _, manager := range flow.managers {
		err := manager.Stop()
		if err != nil {
			return err
		}
	}
	return nil

}
