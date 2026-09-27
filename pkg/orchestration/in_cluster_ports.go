package orchestration

import (
	"context"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

// InClusterPorts is the in-cluster port of each endpoint in a service's network
// mappings, keyed by endpoint name: the port of the endpoint's container-access
// instance, which is the port a deploy hands the service's agent to publish as
// its Kubernetes Service port. An endpoint with no container-access instance —
// an external endpoint that resolves to a public host — is not rendered
// in-cluster and is absent.
func InClusterPorts(ctx context.Context, mappings []*basev0.NetworkMapping) map[string]uint32 {
	ports := make(map[string]uint32, len(mappings))
	for _, mapping := range mappings {
		endpoint := mapping.GetEndpoint()
		if endpoint == nil {
			continue
		}
		instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, mappings, endpoint, resources.NewContainerNetworkAccess())
		if err != nil || instance == nil || instance.GetPort() == 0 {
			continue
		}
		ports[endpoint.GetName()] = instance.GetPort()
	}
	return ports
}

// InClusterPorts returns, per deployed service unique, InClusterPorts over the
// network mappings its deploy recorded — the ports its agent was handed.
func (flow *Flow) InClusterPorts(ctx context.Context) map[string]map[string]uint32 {
	ports := map[string]map[string]uint32{}
	if flow == nil || flow.world == nil || flow.world.SharedState == nil {
		return ports
	}
	for _, unique := range flow.OrderedServiceUniques() {
		mappings, ok := flow.world.SharedState.GetNetworkMappingsFromUnique(unique)
		if !ok {
			continue
		}
		ports[unique] = InClusterPorts(ctx, mappings)
	}
	return ports
}
