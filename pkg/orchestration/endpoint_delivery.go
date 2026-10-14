package orchestration

import (
	"context"
	"fmt"
	"strings"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
)

// EndpointDelivery resolves only the host routes owned by this presence, using
// the same visibility and network-mapping derivation as ordinary carriers.
// It is called after delivery settles the generation, not when the host boots.
func (flow *Flow) EndpointDelivery(ctx context.Context, document *solutionhost.SolutionHostBinding, access *basev0.NetworkAccess) ([]byte, error) {
	if err := document.Validate(); err != nil {
		return nil, err
	}
	if flow == nil || flow.world == nil || flow.world.Env == nil || flow.world.Env.Host == nil {
		return nil, fmt.Errorf("endpoint delivery requires the composition's host")
	}
	world := flow.world
	hostModule, hostService, ok := strings.Cut(world.Env.Host.Gateway, "/")
	if !ok || hostModule == "" || hostService == "" {
		return nil, fmt.Errorf("endpoint delivery requires the host gateway")
	}
	gateway, err := world.Workspace.LoadService(ctx, &resources.ServiceWithModule{Module: hostModule, Name: hostService})
	if err != nil {
		return nil, err
	}
	routes, err := world.hostRoutes(ctx, hostModule)
	if err != nil {
		return nil, err
	}
	carriers := map[string]string{}
	for _, route := range routes {
		for _, endpoint := range document.Endpoints {
			if endpoint.Module != route.module || endpoint.Service != route.service || endpoint.Name != route.endpoint {
				continue
			}
			key, value, err := world.hostRouteCarrier(ctx, gateway, hostModule, route, access)
			if err != nil {
				return nil, fmt.Errorf("deliver %s: %w", route.describe(), err)
			}
			carriers[key] = value
		}
	}
	return solutionhost.MarshalEndpointDelivery(document, carriers)
}

// EndpointCarriers resolves the carriers of every endpoint of one component
// the host routes, at the given access: what a render records for the
// component's presence, so that a publication can bind them to the settled
// generation and deliver them. Keyed by the carrier's environment key, as
// the gateway reads them; a composition without a host or a gateway has
// none. The same routes, visibility and address derivation as the gateway's
// own startup carriers, never a second rule.
func (flow *Flow) EndpointCarriers(ctx context.Context, module string, access *basev0.NetworkAccess) (map[string]string, error) {
	if flow == nil || flow.world == nil || flow.world.Env == nil || flow.world.Env.Host == nil || flow.world.Env.Host.Gateway == "" {
		return nil, nil
	}
	world := flow.world
	hostModule, hostService, ok := strings.Cut(world.Env.Host.Gateway, "/")
	if !ok || hostModule == "" || hostService == "" {
		return nil, fmt.Errorf("endpoint carriers require the host gateway")
	}
	gateway, err := world.Workspace.LoadService(ctx, &resources.ServiceWithModule{Module: hostModule, Name: hostService})
	if err != nil {
		return nil, err
	}
	routes, err := world.hostRoutes(ctx, hostModule)
	if err != nil {
		return nil, err
	}
	carriers := map[string]string{}
	for _, route := range routes {
		if route.module != module {
			continue
		}
		key, value, err := world.hostRouteCarrier(ctx, gateway, hostModule, route, access)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", route.describe(), err)
		}
		carriers[key] = value
	}
	return carriers, nil
}
