package runner

//
//var currentFlow *Flow
//
//func CurrentFlow() *Flow {
//	return currentFlow
//}
//
//type Flow struct {
//	managers     []*manager.Manager
//	dependencies *architecture.Graph
//	provider     *providers.Provider
//
//	actions chan manager.Action
//
//	endpoints       map[string][]*basev0.Endpoint
//	networkMappings map[string][]*basev0.NetworkMapping
//
//	initOnly   bool
//	standAlone bool
//
//	// convenient
//	services map[string]*configurations.Service
//}
//
//func (flow *Flow) Managers(action manager.Action) []*manager.Manager {
//	if action.Unique == "" {
//		return flow.managers
//	}
//	if action.Only {
//		return []*manager.Manager{flow.Manager(action)}
//	}
//	for i, manager := range flow.managers {
//		if manager.Unique() == action.Unique {
//			return flow.managers[i:]
//		}
//	}
//	return nil
//}
//
//func (flow *Flow) Manager(action manager.Action) *manager.Manager {
//	for _, manager := range flow.managers {
//		if manager.Unique() == action.Unique {
//			return manager
//		}
//	}
//	return nil
//}
//
//func NewFlow(ctx context.Context, project *configurations.Project, service *configurations.Service, standAlone bool) (*Flow, error) {
//	w := wool.Get(ctx).In("NewFlow")
//
//	services := map[string]*configurations.Service{service.Unique(): service}
//
//	prov, err := providers.New(ctx, project)
//	if err != nil {
//		return nil, w.Wrap(err)
//	}
//
//	// TODO: Playbook implementation
//	actions := make(chan manager.Action, 100)
//
//	// Get dependency graph
//	g, err := architecture.LoadServiceGraph(ctx, project)
//	if err != nil {
//		return nil, w.Wrap(err)
//	}
//
//	// Create manager for all services required by this service if not standalone
//	var required []string
//	if !standAlone {
//		required = g.TopologicalSortFrom(service.Unique())
//		w.Debug("service dependencies", wool.NameField(service.Name), wool.Field("dependencies", required))
//		cli.Info("Running <%s> with these dependent services: %s", service.Name, strings.Join(required, ", "))
//	}
//	// We run in the proper order
//	slices.Reverse(required)
//
//	var managers []*manager.Manager
//
//	for _, unique := range required {
//		cli.RegisterLoggingResource(unique)
//		info, err := configurations.ParseServiceUnique(unique)
//		w.Debug("creating run manager", wool.Field("for", unique))
//		if err != nil {
//			return nil, w.Wrap(err)
//		}
//		app, err := project.LoadApplicationFromName(ctx, info.Application)
//		if err != nil {
//			return nil, w.Wrap(err)
//		}
//		svc, err := app.LoadServiceFromName(ctx, info.Name)
//		if err != nil {
//			return nil, w.Wrap(err)
//		}
//		services[unique] = svc
//		manager, err := manager.New(ctx, svc, actions)
//		if err != nil {
//			return nil, w.Wrap(err)
//		}
//		managers = append(managers, manager)
//	}
//
//	// Now add the current one
//
//	w.Info("creating run manager", wool.Field("for", service.Unique()))
//	manager, err := manager.New(ctx, service, actions)
//	cli.RegisterLoggingResource(service.Unique())
//	if err != nil {
//		return nil, w.Wrap(err)
//	}
//	managers = append(managers, manager)
//
//	flow := &Flow{
//		managers:        managers,
//		services:        services,
//		dependencies:    g,
//		provider:        prov,
//		actions:         actions,
//		endpoints:       make(map[string][]*basev0.Endpoint),
//		networkMappings: make(map[string][]*basev0.NetworkMapping),
//	}
//	var orders []string
//	for _, m := range managers {
//		orders = append(orders, m.Unique())
//	}
//	w.Debug("running", wool.Field("order", orders))
//	currentFlow = flow
//	return flow, nil
//}
//
//func (action manager.Action) To(t manager.ActionType) manager.Action {
//	return manager.Action{Type: t, Unique: action.Unique, Only: action.Only}
//}
//
//// Start for the  works exactly like the Manager except we don't run all managers, only the required ones
//// TODO: Fix logic with unit tests
//func (flow *Flow) Start(ctx context.Context) error {
//	w := wool.Get(ctx).In("flow.Start")
//	w.Debug("sending init")
//	flow.actions <- manager.Action{Type: manager.Load}
//	for {
//		select {
//		case action := <-flow.actions:
//			switch action.Type {
//			case manager.Noop:
//				w.Debug("received noop")
//			case manager.Load:
//				w.Debug("received load")
//				err := flow.Load(ctx, action)
//				if err != nil {
//					return w.Wrapf(err, "cannot load service")
//				}
//				flow.actions <- action.To(manager.Init)
//			case manager.Init:
//				w.Debug("received init", wool.Field("action", action))
//				err := flow.Init(ctx, action)
//				if err != nil {
//					w.Debug("cannot initialize service", wool.ErrField(err))
//				} else if flow.initOnly {
//					w.Debug("not doing anything")
//				} else {
//					w.Debug("sending start")
//					flow.actions <- action.To(manager.Start)
//				}
//			case manager.Start:
//				w.Debug("received start")
//				err := flow.Start(ctx, action)
//				if err != nil {
//					w.Debug("cannot start service", wool.ErrField(err))
//				}
//			case manager.Restart:
//				w.Debug("received restart")
//				err := flow.Manager(action).Stop()
//				if err != nil {
//					w.Debug("cannot stop service", wool.ErrField(err))
//				}
//				init := action.To(manager.Init)
//				init.Only = true
//				flow.actions <- init
//			case manager.Stop:
//				err := flow.Stop(action)
//				if err != nil {
//					return w.Wrapf(err, "cannot stop service")
//				}
//
//			default:
//				return w.NewError(fmt.Sprintf("unknown action type: %v", action.Type))
//			}
//		case <-ctx.Done():
//			return flow.Stop(manager.Action{Type: manager.Stop})
//		}
//	}
//}
//
//// Load loads the service
//// Request: No dependencies
//// Response: Endpoints
//func (flow *Flow) Load(ctx context.Context, action manager.Action) error {
//	w := wool.Get(ctx).In("Flow.Load")
//	for _, manager := range flow.Managers(action) {
//		err := manager.Load(ctx)
//		if err != nil {
//			return w.Wrapf(err, "cannot load service <%s>", manager.Unique())
//		}
//		flow.endpoints[manager.Unique()] = manager.loaded.Endpoints
//	}
//	return nil
//}
//
//// Init runs all init
//// Init Request:
//// - dependency endpoints
//// - provider information
//func (flow *Flow) Init(ctx context.Context, action manager.Action) error {
//	w := wool.Get(ctx).In("Flow.Init")
//	for _, manager := range flow.Managers(action) {
//		// Endpoints
//		dependenciesEndpoints, err := flow.DependenciesEndpoints(manager.Unique())
//		if err != nil {
//			return w.Wrapf(err, "cannot get endpoint group for <%s>", manager.Unique())
//		}
//		// Provider InfoSource
//		providerInfos, err := flow.GetProviderInfos(ctx, manager.service)
//		if err != nil {
//			return w.Wrapf(err, "cannot get provider info for <%s>", manager.Unique())
//		}
//		manager.WithEndpointDependencies(dependenciesEndpoints)
//		manager.WithProviderInfos(providerInfos)
//		err = manager.Init(ctx)
//		if err != nil {
//			return w.Wrapf(err, "cannot init service <%s>", manager.Unique())
//		}
//		// Add the Provider Infos from the service
//		if len(manager.init.ServiceProviderInfos) > 0 {
//			flow.provider.Share(ctx, manager.init.ServiceProviderInfos)
//		}
//		flow.SetNetworkMappings(manager.Unique(), manager.init.NetworkMappings)
//		w.Debug("init", wool.Field("for", manager.Unique()), wool.NullableField("endpoint dependencies", configurations.MakeEndpointSummary(dependenciesEndpoints)))
//
//	}
//	return nil
//}
//
//func (flow *Flow) Start(ctx context.Context, action manager.Action) error {
//	w := wool.Get(ctx).In("Flow.Start")
//	for _, manager := range flow.Managers(action) {
//		dependenciesNetworkMappings, err := flow.DependenciesNetworkMappings(manager.Unique())
//		if err != nil {
//			return w.Wrapf(err, "cannot get network mappings for <%s>", manager.Unique())
//		}
//		manager.WithNetworkMappings(dependenciesNetworkMappings)
//		err = manager.Start(ctx)
//		if err != nil {
//			return w.Wrapf(err, "cannot run service <%s>", manager.Unique())
//		}
//		w.Trace("run", wool.Field("for", manager.Unique()), wool.NullableField("network mappings", network.MakeNetworkMappingSummary(dependenciesNetworkMappings)))
//	}
//	return nil
//
//}
//
//func (flow *Flow) Stop(action manager.Action) error {
//	for _, manager := range flow.Managers(action) {
//		err := manager.Stop()
//		if err != nil {
//			cli.Error("cannot stop service <%s>: %v", manager.Unique(), err)
//		}
//	}
//	return nil
//
//}
//
//func (flow *Flow) DependenciesEndpoints(unique string) ([]*basev0.Endpoint, error) {
//	w := wool.Get(context.Background()).In("Flow.DependenciesEndpoints")
//	// Gather all endpoints from the direct dependencies
//	dependencies := flow.dependencies.Parents(unique)
//	var endpoints []*basev0.Endpoint
//	for _, dependency := range dependencies {
//		endpoints = append(endpoints, flow.endpoints[dependency]...)
//	}
//	w.Debug("getting dependencies endpoints", wool.SliceCountField(endpoints), wool.Field("for", unique), wool.NullableField("dependencies", dependencies))
//	return endpoints, nil
//}
//
//func (flow *Flow) DependenciesNetworkMappings(unique string) ([]*basev0.NetworkMapping, error) {
//	w := wool.Get(context.Background()).In("Flow.DependenciesNetworkMappings")
//	// Gather all mappings from the direct dependencies
//	dependencies := flow.dependencies.Parents(unique)
//	var mappings []*basev0.NetworkMapping
//	for _, dependency := range dependencies {
//		mappingsForDependency := flow.GetNetworkMappingsForService(dependency)
//		mappings = append(mappings, mappingsForDependency...)
//	}
//	w.Debug("getting dependencies network mappings", wool.SliceCountField(mappings), wool.Field("for", unique), wool.NullableField("dependencies", dependencies))
//	return mappings, nil
//}
//
//func (flow *Flow) InitOnly(only bool) {
//	flow.initOnly = only
//}
//
//func (flow *Flow) StandAlone(alone bool) {
//	flow.standAlone = alone
//}
//
//func (flow *Flow) GetNetworkMappingsForService(unique string) []*basev0.NetworkMapping {
//	return flow.networkMappings[unique]
//}
//
//func (flow *Flow) GetAddressesForEndpoint(application string, service string, endpoint string) []string {
//	unique := configurations.ServiceUnique(application, service)
//	nm := flow.networkMappings[unique]
//	var addresses []string
//	for _, mapping := range nm {
//		if mapping.Endpoint.Name == endpoint {
//			addresses = append(addresses, mapping.Addresses...)
//		}
//	}
//	return addresses
//}
//
//func (flow *Flow) SetNetworkMappings(unique string, mappings []*basev0.NetworkMapping) {
//	flow.networkMappings[unique] = mappings
//}
//
//// GetProviderInfos get the infos for the service and from all the direct dependencies
//func (flow *Flow) GetProviderInfos(ctx context.Context, service *configurations.Service) ([]*basev0.ProviderInformation, error) {
//	infos, err := flow.provider.GetProviderInformation(ctx, service)
//	if err != nil {
//		return nil, err
//	}
//	for _, dep := range flow.Antecedents(service) {
//		depInfos, err := flow.provider.GetSharedProviderInformation(ctx, dep)
//		if err != nil {
//			return nil, err
//		}
//		infos = append(infos, depInfos...)
//	}
//	return infos, nil
//}
//
//func (flow *Flow) Antecedents(service *configurations.Service) []*configurations.Service {
//	var res []*configurations.Service
//	for _, dep := range flow.dependencies.Parents(service.Unique()) {
//		res = append(res, flow.services[dep])
//	}
//	return res
//}
