package orchestration

import (
	"context"
	"fmt"
	"sort"

	"github.com/codefly-dev/cli/pkg/solutionrun"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// THE HOST GATEWAY'S ROUTES.
//
// A composed solution calls a consumed module through the host's gateway
// (/v1/<as>/…, the solution's api.consumes): the gateway forwards the call on
// the solution's behalf, so it must reach exactly what each solution declares
// it consumes. The Codefly SDK in the gateway answers where an endpoint lives
// from the CODEFLY__ENDPOINT__ carriers the composition wrote into its process,
// so this is where those carriers come from — the same way every carrier
// does: the producer's own network mappings (producerNetworkMappings — its
// deployed in-cluster mappings in a render, its run's mappings locally),
// judged by the producer's manifest for the gateway's module (exportableTo),
// rendered by core's environment-variable manager at the gateway's network
// access. No dependency edge is added: the gateway neither waits on, builds,
// nor deploys a module; it is only told where each consumed one lives.

// hostRouteCarriers returns the gateway's route carriers when service is the
// environment's host gateway, nil otherwise.
func (world *World) hostRouteCarriers(ctx context.Context, service *resources.Service, access *basev0.NetworkAccess) (map[string]string, error) {
	if world == nil || world.Env == nil || world.Env.Host == nil || world.Env.Host.Gateway == "" || service == nil {
		return nil, nil
	}
	identity, err := service.Identity()
	if err != nil || identity.Unique() != world.Env.Host.Gateway {
		return nil, nil
	}
	routes, err := world.hostRoutes(ctx, identity.Module)
	if err != nil {
		return nil, err
	}
	manager := resources.NewEnvironmentVariableManager()
	for _, route := range routes {
		mappings, err := world.producerNetworkMappings(ctx, route.producer())
		if err != nil {
			return nil, fmt.Errorf("derive the address of %s, which %s consumes through the host: %w", route.reference(), route.solution, err)
		}
		var consumed []*basev0.NetworkMapping
		for _, mapping := range mappings {
			if mapping.GetEndpoint().GetName() == route.endpoint {
				consumed = append(consumed, mapping)
			}
		}
		if len(consumed) == 0 {
			if !world.deploys() {
				// A local producer has an address only once this run allocated
				// it (--temporary-ports allocates at initialization), and a
				// route the run cannot address yet is the run being early, not
				// a fault: the gateway answers that module 503 until restarted.
				world.warnRoute(ctx, route, "this run has not allocated its address")
				continue
			}
			return nil, fmt.Errorf("%s consumes %s through the host, and that producer declares no such endpoint", route.solution, route.reference())
		}
		visible, err := world.exportableTo(ctx, service, consumed)
		if err != nil {
			return nil, err
		}
		if len(visible) == 0 {
			return nil, fmt.Errorf("%s consumes %s through the host, whose gateway (module %s) that endpoint's visibility does not admit", route.solution, route.reference(), identity.Module)
		}
		if err := manager.AddEndpoints(ctx, visible, access); err != nil {
			return nil, err
		}
	}
	carriers := map[string]string{}
	for _, endpoint := range manager.Endpoints() {
		variable := resources.EndpointAsEnvironmentVariable(endpoint)
		if endpoint.NetworkInstance.GetAddress() == "" {
			return nil, fmt.Errorf("the host gateway's route %s has no address at its network access", variable.Key)
		}
		carriers[variable.Key] = fmt.Sprint(variable.Value)
	}
	if len(carriers) == 0 {
		return nil, nil
	}
	return carriers, nil
}

// hostRoute is one module endpoint composed solutions consume through the host.
type hostRoute struct {
	solution                  string
	module, service, endpoint string
}

func (route hostRoute) producer() string { return route.module + "/" + route.service }

func (route hostRoute) reference() string {
	return route.module + "/" + route.service + "/" + route.endpoint
}

// hostRoutes is the set of module endpoints the composed solutions consume
// through the host, one per module. The gateway routes /v1/<alias> on the
// module's name, so a consume that names it under another alias, or two
// solutions reaching one module through different endpoints, has no single
// route and is refused rather than chosen between.
func (world *World) hostRoutes(ctx context.Context, hostModule string) ([]hostRoute, error) {
	consumptions, err := solutionrun.ConsumedEndpoints(ctx, world.Workspace)
	if err != nil {
		return nil, err
	}
	byModule := map[string]hostRoute{}
	for index := range consumptions {
		consumed := &consumptions[index]
		module, err := world.Workspace.LoadModuleFromName(ctx, consumed.Module)
		if err != nil {
			return nil, fmt.Errorf("solution %s consumes module %s, which this workspace does not compose: %w", consumed.Solution, consumed.Module, err)
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
	return routes, nil
}

func (world *World) warnRoute(ctx context.Context, route hostRoute, reason string) {
	wool.Get(ctx).In("World.hostRouteCarriers").Warn("the host gateway will not route this consumed endpoint: "+reason,
		wool.Field("endpoint", route.reference()), wool.Field("solution", route.solution))
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
		carriers, err := flow.world.hostRouteCarriers(ctx, service, resources.NewContainerNetworkAccess())
		if err != nil || carriers == nil {
			return nil, err
		}
		return map[string]map[string]string{gateway: carriers}, nil
	}
	return nil, nil
}
