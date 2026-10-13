package orchestration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/solutionrun"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
)

// THE HOST GATEWAY'S ROUTES.
//
// The host's gateway forwards two kinds of call, and resolves both from the
// CODEFLY__ENDPOINT__ carriers the composition wrote into its process, through
// the Codefly SDK:
//
//   - a composed solution calls a consumed module through it (/v1/<as>/…, the
//     solution's api.consumes), so it must reach exactly what each solution
//     declares it consumes;
//   - a browser reaches a solution backend or a module's declared page through it
//     (/<alias>/…), so it must reach each component's entry endpoint — the
//     one HTTP endpoint the solution's entry service declares — in plain
//     HTTP, the way it already reaches a module. Nothing a solution reports
//     at run time is an address: the gateway routes a solution from its
//     applied presence's endpoints and from these carriers, as it routes a
//     module, and a solution's transport identity is the mesh's, not a
//     certificate it serves.
//
// Both come from here the same way every carrier does: the producer's own
// network mappings (producerNetworkMappings — its deployed in-cluster mappings
// in a render, its run's mappings locally), judged by the producer's manifest
// for the gateway's module (exportableTo), rendered by core's
// environment-variable manager at the gateway's network access. No dependency
// edge is added: the gateway neither waits on, builds, nor deploys a module or
// a solution; it is only told where each lives.

// hostRouteCarriers returns the gateway's route carriers when service is the
// environment's host gateway, nil otherwise. Each route is derived on its own:
// a route with no answer is reported in problems and never takes another
// route's carrier with it. err is a refusal of the composition itself (see
// hostRoutes), which no route can be derived past.
func (world *World) hostRouteCarriers(ctx context.Context, service *resources.Service, access *basev0.NetworkAccess) (map[string]string, []error, error) {
	if world == nil || world.Env == nil || world.Env.Host == nil || world.Env.Host.Gateway == "" || service == nil {
		return nil, nil, nil
	}
	identity, err := service.Identity()
	if err != nil || identity.Unique() != world.Env.Host.Gateway {
		return nil, nil, nil
	}
	routes, err := world.hostRoutes(ctx, identity.Module)
	if err != nil {
		return nil, nil, err
	}
	carriers := map[string]string{}
	var problems []error
	for _, route := range routes {
		key, value, err := world.hostRouteCarrier(ctx, service, identity.Module, route, access)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", route.describe(), err))
			continue
		}
		carriers[key] = value
	}
	if len(carriers) == 0 {
		carriers = nil
	}
	return carriers, problems, nil
}

// hostRouteCarrier is one route's carrier, or why it has none.
func (world *World) hostRouteCarrier(
	ctx context.Context, gateway *resources.Service, hostModule string, route hostRoute, access *basev0.NetworkAccess,
) (string, string, error) {
	if _, err := world.Dependencies.ServiceFromUnique(route.producer()); err != nil {
		if world.deploys() {
			return "", "", fmt.Errorf("%s is not a service of this workspace", route.producer())
		}
		return "", "", fmt.Errorf("%s is not part of this run", route.producer())
	}
	mappings, err := world.producerNetworkMappings(ctx, route.producer())
	if err != nil {
		return "", "", fmt.Errorf("derive its address: %w", err)
	}
	var consumed []*basev0.NetworkMapping
	for _, mapping := range mappings {
		if mapping.GetEndpoint().GetName() == route.endpoint {
			consumed = append(consumed, mapping)
		}
	}
	if len(consumed) == 0 {
		if !world.deploys() {
			// A local producer has an address only once this run allocated it
			// (--temporary-ports allocates at initialization).
			return "", "", fmt.Errorf("this run has not allocated %s's address yet", route.producer())
		}
		return "", "", fmt.Errorf("%s declares no endpoint named %q", route.producer(), route.endpoint)
	}
	visible, err := world.exportableTo(ctx, gateway, consumed)
	if err != nil {
		return "", "", err
	}
	if len(visible) == 0 {
		return "", "", fmt.Errorf("its visibility does not admit the host gateway's module %s", hostModule)
	}
	manager := resources.NewEnvironmentVariableManager()
	if err := manager.AddEndpoints(ctx, visible, access); err != nil {
		return "", "", err
	}
	endpoints := manager.Endpoints()
	if len(endpoints) != 1 || endpoints[0].NetworkInstance.GetAddress() == "" {
		return "", "", fmt.Errorf("it has no address at the host gateway's network access")
	}
	variable := resources.EndpointAsEnvironmentVariable(endpoints[0])
	return variable.Key, fmt.Sprint(variable.Value), nil
}

// hostRoute is one endpoint the host's gateway forwards to: a module endpoint
// composed solutions consume through the host, or a composed solution's own
// backend endpoint (backend is true).
type hostRoute struct {
	solution                  string
	module, service, endpoint string
	backend                   bool
}

func (route hostRoute) producer() string { return route.module + "/" + route.service }

func (route hostRoute) reference() string {
	return route.module + "/" + route.service + "/" + route.endpoint
}

// describe names the route the way a reader of a refusal looks for it.
func (route hostRoute) describe() string {
	if route.backend {
		return fmt.Sprintf("the host fronts component %s at its backend endpoint %s", route.solution, route.reference())
	}
	return fmt.Sprintf("%s consumes %s through the host", route.solution, route.reference())
}

// hostRoutes is the set of endpoints the host's gateway forwards to: the
// module endpoints the composed solutions consume through it, one per module,
// then each composed solution's backend endpoint. The gateway routes
// /v1/<alias> on the module's name, so a consume that names it under another
// alias, or two solutions reaching one module through different endpoints,
// has no single route and is refused rather than chosen between; and it
// routes /<alias>/ to one HTTP endpoint of the solution's entry service, so an
// entry declaring none or several is refused the same way.
func (world *World) hostRoutes(ctx context.Context, hostModule string) ([]hostRoute, error) {
	return hostRoutes(ctx, world.Workspace, hostModule)
}

// HostRouteTarget is one endpoint the host's gateway forwards to, as the
// composition declares it: a module endpoint a solution consumes through the
// host, or (Backend) a composed solution's own backend endpoint.
type HostRouteTarget struct {
	Solution string
	Module   string
	Service  string
	Endpoint string
	Backend  bool
}

// HostRouteTargets is the set of endpoints the host's gateway forwards to,
// derived from the composition alone (hostRoutes) — for whoever needs the
// edges without an address, such as the mesh edges a render delivers.
func HostRouteTargets(ctx context.Context, workspace *resources.Workspace, hostModule string) ([]HostRouteTarget, error) {
	routes, err := hostRoutes(ctx, workspace, hostModule)
	if err != nil {
		return nil, err
	}
	targets := make([]HostRouteTarget, 0, len(routes))
	for _, route := range routes {
		targets = append(targets, HostRouteTarget{Solution: route.solution, Module: route.module, Service: route.service, Endpoint: route.endpoint, Backend: route.backend})
	}
	return targets, nil
}

func hostRoutes(ctx context.Context, workspace *resources.Workspace, hostModule string) ([]hostRoute, error) {
	consumptions, err := solutionrun.ConsumedEndpoints(ctx, workspace)
	if err != nil {
		return nil, err
	}
	byModule := map[string]hostRoute{}
	for index := range consumptions {
		consumed := &consumptions[index]
		module, loadErr := workspace.LoadModuleFromName(ctx, consumed.Module)
		if loadErr != nil {
			return nil, fmt.Errorf("solution %s consumes module %s, which this workspace does not compose: %w", consumed.Solution, consumed.Module, loadErr)
		}
		if module.Name == hostModule || module.Name == consumed.Solution {
			// The host's own endpoints and a solution's own are not routed
			// through /v1/<alias>.
			continue
		}
		if consumed.As != "" && consumed.As != module.Name {
			return nil, fmt.Errorf("solution %s consumes %s as %q; the host routes a module at /v1/<module>, so it must be consumed as %q", consumed.Solution, module.Name, consumed.As, module.Name)
		}
		route := hostRoute{solution: consumed.Solution, module: module.Name, service: consumed.Service, endpoint: consumed.Endpoint}
		if previous, seen := byModule[module.Name]; seen {
			if previous.service != route.service || previous.endpoint != route.endpoint {
				return nil, fmt.Errorf("solutions %s and %s reach module %s through different endpoints (%s, %s); the host routes /v1/%s to one",
					previous.solution, route.solution, module.Name, previous.reference(), route.reference(), module.Name)
			}
			continue
		}
		byModule[module.Name] = route
	}
	routes := make([]hostRoute, 0, len(byModule))
	for _, route := range byModule {
		routes = append(routes, route)
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].module < routes[j].module })
	backends, err := componentBackendRoutes(ctx, workspace, hostModule)
	if err != nil {
		return nil, err
	}
	for _, backend := range backends {
		if consumed, exists := byModule[backend.module]; exists && consumed.reference() != backend.reference() {
			return nil, fmt.Errorf("component %s's page uses %s but its consumed API uses %s; the host requires one shared upstream", backend.module, backend.reference(), consumed.reference())
		}
	}
	return append(routes, backends...), nil
}

// componentBackendRoutes selects each solution backend and each module page's
// entry endpoint. A page must be reachable even if no solution consumes its API.
// The host is not fronted through itself.
func componentBackendRoutes(ctx context.Context, workspace *resources.Workspace, hostModule string) ([]hostRoute, error) {
	solutions, err := solutionrun.ComposedSolutions(ctx, workspace)
	if err != nil {
		return nil, err
	}
	isSolution := make(map[string]bool, len(solutions))
	for _, solution := range solutions {
		isSolution[solution.Name] = true
	}
	var routes []hostRoute
	for _, ref := range workspace.Modules {
		component, err := workspace.LoadModuleFromReference(ctx, ref)
		if err != nil {
			return nil, err
		}
		if component.Name == hostModule {
			continue
		}
		if !isSolution[component.Name] {
			frontend, err := solutionrun.DeclaredFrontend(ctx, component)
			if err != nil {
				return nil, err
			}
			if frontend == nil {
				continue
			}
		}
		entry, err := component.LoadServiceFromName(ctx, component.ServiceEntry)
		if err != nil {
			return nil, fmt.Errorf("component %s names %s as its service-entry: %w", component.Name, component.ServiceEntry, err)
		}
		var candidates []string
		for _, endpoint := range entry.Endpoints {
			if endpoint != nil && (endpoint.API == standards.HTTP || endpoint.API == standards.REST) {
				candidates = append(candidates, endpoint.Name)
			}
		}
		switch len(candidates) {
		case 1:
			routes = append(routes, hostRoute{solution: component.Name, module: component.Name, service: entry.Name, endpoint: candidates[0], backend: true})
		case 0:
			return nil, fmt.Errorf("component %s's entry service %s declares no http or rest endpoint; the host forwards /%s/ to one", component.Name, entry.Name, component.Name)
		default:
			return nil, fmt.Errorf("component %s's entry service %s declares several http or rest endpoints (%s); the host forwards /%s/ to one, and which has no single answer", component.Name, entry.Name, strings.Join(candidates, ", "), component.Name)
		}
	}
	return routes, nil
}

// HostRouteCarriers is hostRouteCarriers for a render: the gateway's route
// carriers at container access, keyed by the gateway's unique, when this flow
// deploys it.
func (flow *Flow) HostRouteCarriers(ctx context.Context) (map[string]map[string]string, error) {
	if flow == nil || flow.world == nil || flow.world.Env == nil || flow.world.Env.Host == nil || flow.world.Env.Host.Gateway == "" {
		return nil, nil
	}
	gateway := flow.world.Env.Host.Gateway
	for _, unique := range flow.OrderedServiceUniques() {
		if unique != gateway {
			continue
		}
		service, err := flow.world.Dependencies.ServiceFromUnique(unique)
		if err != nil {
			return nil, err
		}
		carriers, problems, err := flow.world.hostRouteCarriers(ctx, service, resources.NewContainerNetworkAccess())
		if err != nil {
			return nil, err
		}
		// A render is committed: a route it cannot derive is refused, never
		// shipped as a gateway that answers that module 503.
		if len(problems) > 0 {
			return nil, errors.Join(problems...)
		}
		if carriers == nil {
			return nil, nil
		}
		return map[string]map[string]string{gateway: carriers}, nil
	}
	return nil, nil
}
