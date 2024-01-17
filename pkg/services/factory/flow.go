package factory

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
	"github.com/codefly-dev/core/wool"
)

type Flow struct {
	managers     []*Manager
	dependencies *architecture.Graph
	endpoints    map[string][]*basev0.Endpoint
	actions      chan Action
	initOnly     bool
}

func (flow *Flow) Managers(action Action) []*Manager {
	if action.Unique == "" {
		return flow.managers
	}
	if action.Only {
		return []*Manager{flow.Manager(action)}
	}
	for i, manager := range flow.managers {
		if manager.Unique() == action.Unique {
			return flow.managers[i:]
		}
	}
	return nil
}

func (flow *Flow) Manager(action Action) *Manager {
	for _, manager := range flow.managers {
		if manager.Unique() == action.Unique {
			return manager
		}
	}
	return nil
}

func NewFlow(ctx context.Context, project *configurations.Project, service *configurations.Service) (*Flow, error) {
	w := wool.Get(ctx).In("NewFactoryFlow")
	// Get dependency graph
	g, err := architecture.LoadServiceGraph(ctx, project)
	if err != nil {
		return nil, w.Wrap(err)
	}
	// Create manager for all services required by this service
	uniques := g.TopologicalSortFrom(service.Unique())
	w.Debug("service dependencies", wool.NameField(service.Name), wool.Field("dependencies", uniques))

	slices.Reverse(uniques)

	var managers []*Manager

	for _, unique := range uniques {
		info, err := configurations.ParseServiceUnique(unique)
		w.Debug("creating factory manager", wool.Field("for", unique))
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

	w.Info("creating flow manager", wool.Field("for", service.Unique()))
	manager, err := New(ctx, service)
	if err != nil {
		return nil, w.Wrap(err)
	}
	managers = append(managers, manager)
	if len(uniques) > 0 {
		cli.Info("Factoring <%s> with these dependent services: %s", service.Name, strings.Join(uniques, ", "))
	} else {
		cli.Info("Factoring <%s> with no dependent services", service.Name)
	}
	return &Flow{
		managers:     managers,
		endpoints:    make(map[string][]*basev0.Endpoint),
		dependencies: g,
		actions:      make(chan Action, 1),
	}, nil
}

func (action Action) To(t ActionType) Action {
	return Action{Type: t, Unique: action.Unique, Only: action.Only}
}

func (flow *Flow) Start(ctx context.Context, afterLoad ActionType) error {
	w := wool.Get(ctx).In("flow")
	w.Debug("sending init")
	flow.actions <- Action{Type: Load}
	for {
		select {
		case action := <-flow.actions:
			switch action.Type {
			case Noop:
				w.Debug("received noop")
			case Load:
				err := flow.Load(ctx)
				if err != nil {
					return w.Wrapf(err, "cannot load service")
				}
				flow.actions <- action.To(Init)
			case Init:
				w.Debug("received init")
				err := flow.Init(ctx)
				if err != nil {
					return w.Wrapf(err, "cannot init service")
				} else if flow.initOnly {
					return nil
				} else {
					w.Debug("sending start")
					flow.actions <- action.To(afterLoad)
				}
			case Build:
				w.Debug("received build")
				err := flow.Build(ctx, action)
				if err != nil {
					return w.Wrapf(err, "cannot build service")
				}
				return nil
			case Sync:
				w.Debug("received sync")
				err := flow.Sync(ctx)
				if err != nil {
					return w.Wrapf(err, "cannot sync service")
				}
				return nil
			default:
				return w.NewError(fmt.Sprintf("unknown action type: <%v>", action.Type))
			}
		case <-ctx.Done():
			return flow.Stop()
		}
	}
}

// Load loads the service
// Request: No dependencies
// Response: Endpoints
func (flow *Flow) Load(ctx context.Context) error {
	w := wool.Get(ctx).In("Flow.Load")
	for _, manager := range flow.managers {
		err := manager.Load(ctx)
		if err != nil {
			return w.Wrapf(err, "cannot load service <%s>", manager.instance.Service.Unique())
		}
		flow.endpoints[manager.Unique()] = manager.loaded.Endpoints
	}
	return nil
}

// Init runs all init
// Init Request: Endpoint group
func (flow *Flow) Init(ctx context.Context) error {
	w := wool.Get(ctx).In("Flow.Init")
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
	}
	return nil
}

func (flow *Flow) Sync(ctx context.Context) error {
	w := wool.Get(ctx).In("Flow.Sync")
	for _, manager := range flow.managers {
		err := manager.Sync(ctx)
		if err != nil {
			return w.Wrapf(err, "cannot run service <%s>", manager.Unique())
		}
		w.Debug("run", wool.Field("for", manager.Unique()))
	}
	return nil

}

func (flow *Flow) Build(ctx context.Context, action Action) error {
	w := wool.Get(ctx).In("Flow.Build")
	for _, manager := range flow.Managers(action) {
		err := manager.Build(ctx)
		if err != nil {
			return w.Wrapf(err, "cannot build service <%s>", manager.Unique())
		}
	}
	return nil
}

func (flow *Flow) Stop() error {
	//for _, manager := range flow.managers {
	//	err := manager.Stop()
	//	if err != nil {
	//		return err
	//	}
	//}
	return nil
}

func (flow *Flow) DependenciesEndpoints(unique string) ([]*basev0.Endpoint, error) {
	w := wool.Get(context.Background()).In("Flow.DependenciesEndpoints")
	// Gather all endpoints from the direct dependencies
	dependencies := flow.dependencies.Antecedents(unique)
	var endpoints []*basev0.Endpoint
	for _, dependency := range dependencies {
		endpoints = append(endpoints, flow.endpoints[dependency]...)
	}
	w.Debug("getting dependencies endpoints", wool.SliceCountField(endpoints), wool.Field("for", unique), wool.NullableField("dependencies", dependencies))
	return endpoints, nil
}

func (flow *Flow) DependenciesNetworkMappings(unique string) ([]*runtimev0.NetworkMapping, error) {
	w := wool.Get(context.Background()).In("Flow.DependenciesNetworkMappings")
	// Gather all mappings from the direct dependencies
	dependencies := flow.dependencies.Antecedents(unique)
	var mappings []*runtimev0.NetworkMapping
	for _, dependency := range dependencies {
		mappingsForDependency := GetNetworkMappingsForService(dependency)
		mappings = append(mappings, mappingsForDependency...)
	}
	w.Debug("getting dependencies network mappings", wool.SliceCountField(mappings), wool.Field("for", unique), wool.NullableField("dependencies", dependencies))
	return mappings, nil
}

func (flow *Flow) InitOnly(only bool) {
	flow.initOnly = only
}
