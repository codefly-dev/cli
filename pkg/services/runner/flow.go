package runner

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/provider"
	"github.com/codefly-dev/core/agents/network"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
	"github.com/codefly-dev/core/wool"
)

var currentFlow *Flow

func CurrentFlow() *Flow {
	return currentFlow
}

type Flow struct {
	managers     []*Manager
	dependencies *architecture.Graph
	provider     *provider.Provider

	actions chan Action

	endpoints       map[string][]*basev0.Endpoint
	networkMappings map[string][]*runtimev0.NetworkMapping

	initOnly   bool
	standAlone bool

	// convenient
	services map[string]*configurations.Service
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

func NewFlow(ctx context.Context, project *configurations.Project, service *configurations.Service, standAlone bool) (*Flow, error) {
	w := wool.Get(ctx).In("NewFlow")

	services := map[string]*configurations.Service{service.Unique(): service}

	prov, err := provider.New(ctx, project)
	if err != nil {
		return nil, w.Wrap(err)
	}

	// TODO: Playbook implementation
	actions := make(chan Action, 100)

	// Get dependency graph
	g, err := architecture.LoadServiceGraph(ctx, project)
	if err != nil {
		return nil, w.Wrap(err)
	}

	// Create manager for all services required by this service if not standalone
	var required []string
	if !standAlone {
		required = g.TopologicalSortFrom(service.Unique())
		w.Debug("service dependencies", wool.NameField(service.Name), wool.Field("dependencies", required))
		cli.Info("Running <%s> with these dependent services: %s", service.Name, strings.Join(required, ", "))
	}
	// We run in the proper order
	slices.Reverse(required)

	var managers []*Manager

	for _, unique := range required {
		cli.RegisterLoggingResource(unique)
		info, err := configurations.ParseServiceUnique(unique)
		w.Debug("creating run manager", wool.Field("for", unique))
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
		services[unique] = svc
		manager, err := New(ctx, svc, actions)
		if err != nil {
			return nil, w.Wrap(err)
		}
		managers = append(managers, manager)
	}

	// Now add the current one

	w.Info("creating run manager", wool.Field("for", service.Unique()))
	manager, err := New(ctx, service, actions)
	cli.RegisterLoggingResource(service.Unique())
	if err != nil {
		return nil, w.Wrap(err)
	}
	managers = append(managers, manager)

	flow := &Flow{
		managers:        managers,
		services:        services,
		dependencies:    g,
		provider:        prov,
		actions:         actions,
		endpoints:       make(map[string][]*basev0.Endpoint),
		networkMappings: make(map[string][]*runtimev0.NetworkMapping),
	}
	var orders []string
	for _, m := range managers {
		orders = append(orders, m.Unique())
	}
	w.Debug("running", wool.Field("order", orders))
	currentFlow = flow
	return flow, nil
}

func (action Action) To(t ActionType) Action {
	return Action{Type: t, Unique: action.Unique, Only: action.Only}
}

// Start for the  works exactly like the Manager except we don't run all managers, only the required ones
// TODO: Fix logic with unit tests
func (flow *Flow) Start(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.Start")
	w.Debug("sending init")
	flow.actions <- Action{Type: Load}
	for {
		select {
		case action := <-flow.actions:
			switch action.Type {
			case Noop:
				w.Debug("received noop")
			case Load:
				w.Debug("received load")
				err := flow.Load(ctx, action)
				if err != nil {
					return w.Wrapf(err, "cannot load service")
				}
				flow.actions <- action.To(Init)
			case Init:
				w.Debug("received init", wool.Field("action", action))
				err := flow.Init(ctx, action)
				if err != nil {
					w.Debug("cannot initialize service", wool.ErrField(err))
				} else if flow.initOnly {
					w.Debug("not doing anything")
				} else {
					w.Debug("sending start")
					flow.actions <- action.To(Start)
				}
			case Start:
				w.Debug("received start")
				err := flow.Run(ctx, action)
				if err != nil {
					w.Debug("cannot start service", wool.ErrField(err))
				}
			case Restart:
				w.Debug("received restart")
				err := flow.Manager(action).Stop()
				if err != nil {
					w.Debug("cannot stop service", wool.ErrField(err))
				}
				init := action.To(Init)
				init.Only = true
				flow.actions <- init
			case Stop:
				err := flow.Stop(action)
				if err != nil {
					return w.Wrapf(err, "cannot stop service")
				}

			default:
				return w.NewError(fmt.Sprintf("unknown action type: %v", action.Type))
			}
		case <-ctx.Done():
			return flow.Stop(Action{Type: Stop})
		}
	}
}

// Load loads the service
// Request: No dependencies
// Response: Endpoints
func (flow *Flow) Load(ctx context.Context, action Action) error {
	w := wool.Get(ctx).In("Flow.Load")
	for _, manager := range flow.Managers(action) {
		err := manager.Load(ctx)
		if err != nil {
			return w.Wrapf(err, "cannot load service <%s>", manager.Unique())
		}
		flow.endpoints[manager.Unique()] = manager.loaded.Endpoints
	}
	return nil
}

// Init runs all init
// Init Request:
// - dependency endpoints
// - provider information
func (flow *Flow) Init(ctx context.Context, action Action) error {
	w := wool.Get(ctx).In("Flow.Init")
	for _, manager := range flow.Managers(action) {
		// Endpoints
		dependenciesEndpoints, err := flow.DependenciesEndpoints(manager.Unique())
		if err != nil {
			return w.Wrapf(err, "cannot get endpoint group for <%s>", manager.Unique())
		}
		// Provider InfoSource
		providerInfos, err := flow.GetProviderInfos(ctx, manager.service)
		if err != nil {
			return w.Wrapf(err, "cannot get provider info for <%s>", manager.Unique())
		}
		if len(providerInfos) > 0 {
			w.Focus("SHARING provider infos", wool.Field("for", manager.Unique()))
		}
		manager.WithEndpointDependencies(dependenciesEndpoints)
		manager.WithProviderInfos(providerInfos)
		err = manager.Init(ctx)
		if err != nil {
			return w.Wrapf(err, "cannot init service <%s>", manager.Unique())
		}
		// Add the Provider Infos from the service
		if len(manager.init.ServiceProviderInfos) > 0 {
			w.Focus("EXPORTING provider infos", wool.Field("for", manager.Unique()))
			flow.provider.Share(ctx, manager.init.ServiceProviderInfos)
		}
		flow.SetNetworkMappings(manager.Unique(), manager.init.NetworkMappings)
		w.Debug("init", wool.Field("for", manager.Unique()), wool.NullableField("endpoint dependencies", configurations.MakeEndpointSummary(dependenciesEndpoints)))

	}
	return nil
}

func (flow *Flow) Run(ctx context.Context, action Action) error {
	w := wool.Get(ctx).In("Flow.Run")
	for _, manager := range flow.Managers(action) {
		dependenciesNetworkMappings, err := flow.DependenciesNetworkMappings(manager.Unique())
		if err != nil {
			return w.Wrapf(err, "cannot get network mappings for <%s>", manager.Unique())
		}
		manager.WithNetworkMappings(dependenciesNetworkMappings)
		err = manager.Run(ctx)
		if err != nil {
			return w.Wrapf(err, "cannot run service <%s>", manager.Unique())
		}
		w.Trace("run", wool.Field("for", manager.Unique()), wool.NullableField("network mappings", network.MakeNetworkMappingSummary(dependenciesNetworkMappings)))
	}
	return nil

}

func (flow *Flow) Stop(action Action) error {
	for _, manager := range flow.Managers(action) {
		err := manager.Stop()
		if err != nil {
			cli.Error("cannot stop service <%s>: %v", manager.Unique(), err)
		}
	}
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
		mappingsForDependency := flow.GetNetworkMappingsForService(dependency)
		mappings = append(mappings, mappingsForDependency...)
	}
	w.Debug("getting dependencies network mappings", wool.SliceCountField(mappings), wool.Field("for", unique), wool.NullableField("dependencies", dependencies))
	return mappings, nil
}

func (flow *Flow) InitOnly(only bool) {
	flow.initOnly = only
}

func (flow *Flow) StandAlone(alone bool) {
	flow.standAlone = alone
}

func (flow *Flow) GetNetworkMappingsForService(unique string) []*runtimev0.NetworkMapping {
	return flow.networkMappings[unique]
}

func (flow *Flow) GetAddressesForEndpoint(application string, service string, endpoint string) []string {
	unique := configurations.ServiceUnique(application, service)
	nm := flow.networkMappings[unique]
	var addresses []string
	for _, mapping := range nm {
		if mapping.Endpoint.Name == endpoint {
			addresses = append(addresses, mapping.Addresses...)
		}
	}
	return addresses
}

func (flow *Flow) SetNetworkMappings(unique string, mappings []*runtimev0.NetworkMapping) {
	flow.networkMappings[unique] = mappings
}

// GetProviderInfos get the infos for the service and from all the direct dependencies
func (flow *Flow) GetProviderInfos(ctx context.Context, service *configurations.Service) ([]*basev0.ProviderInformation, error) {
	infos, err := flow.provider.GetProviderInformation(ctx, service)
	if err != nil {
		return nil, err
	}
	for _, dep := range flow.Antecedents(service) {
		depInfos, err := flow.provider.GetSharedProviderInformation(ctx, dep)
		if err != nil {
			return nil, err
		}
		infos = append(infos, depInfos...)
	}
	return infos, nil
}

func (flow *Flow) Antecedents(service *configurations.Service) []*configurations.Service {
	var res []*configurations.Service
	for _, dep := range flow.dependencies.Antecedents(service.Unique()) {
		res = append(res, flow.services[dep])
	}
	return res
}
