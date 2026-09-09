package orchestration

import (
	"context"
	"fmt"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"google.golang.org/protobuf/proto"
)

// acceptNetworkMappings turns an agent's Init response into the one set of
// network mappings the CLI publishes: the runner, the shared state consumers
// read (dependents' Start requests, readiness, the dashboard) and the exported
// environment all take this value.
//
// The agent is authoritative — it may bind a port other than the one core
// proposed — but only within the shape of the proposal. The accepted set must
// cover exactly the proposed endpoints, each owned by this service, each
// carrying exactly the access views (native/container/public) that were
// proposed, and each view must stay distinct where the proposal made it so.
// Anything else fails Init, before a single value is published.
//
// An empty response is the legacy path: InitResponse.network_mappings postdates
// the first agents, so an agent that returns none is taken to accept the
// proposal, which is then adopted whole. A partial response is never merged
// with the proposal endpoint by endpoint.
func acceptNetworkMappings(ctx context.Context, identity *resources.ServiceIdentity, proposed, returned []*basev0.NetworkMapping) ([]*basev0.NetworkMapping, error) {
	w := wool.Get(ctx).In("orchestration.acceptNetworkMappings", wool.ThisField(identity))
	if len(returned) == 0 {
		if len(proposed) > 0 {
			w.Warn("agent returned no network mappings: accepting the proposed mappings unchanged (legacy Init contract)")
		}
		return cloneNetworkMappings(proposed), nil
	}

	proposals := make(map[string]*basev0.NetworkMapping, len(proposed))
	for _, mapping := range proposed {
		proposals[resources.EndpointDestination(mapping.GetEndpoint())] = mapping
	}

	accepted := make(map[string]bool, len(returned))
	for _, mapping := range returned {
		endpoint := mapping.GetEndpoint()
		if endpoint == nil {
			return nil, w.NewError("accepted network mapping has no endpoint")
		}
		destination := resources.EndpointDestination(endpoint)
		if endpoint.GetModule() != identity.Module || endpoint.GetService() != identity.Name {
			return nil, w.NewError("accepted network mapping for %s is not owned by %s", destination, identity.Unique())
		}
		proposal, ok := proposals[destination]
		if !ok {
			return nil, w.NewError("accepted network mapping for %s was never proposed", destination)
		}
		if accepted[destination] {
			return nil, w.NewError("accepted network mappings contain %s twice", destination)
		}
		accepted[destination] = true
		if err := acceptNetworkInstances(destination, proposal, mapping); err != nil {
			return nil, w.Wrap(err)
		}
	}
	for _, mapping := range proposed {
		destination := resources.EndpointDestination(mapping.GetEndpoint())
		if !accepted[destination] {
			return nil, w.NewError("accepted network mappings omit proposed endpoint %s", destination)
		}
	}
	return cloneNetworkMappings(returned), nil
}

// acceptNetworkInstances checks the addresses an agent accepted for one
// endpoint: every proposed access view is answered exactly once with a
// complete address, no view is invented (which is how a private endpoint would
// gain a public address), and a container view that the proposal kept distinct
// from the host view does not collapse onto the host address.
func acceptNetworkInstances(destination string, proposed, returned *basev0.NetworkMapping) error {
	views := make(map[string]*basev0.NetworkInstance, len(returned.GetInstances()))
	for _, instance := range returned.GetInstances() {
		access := instance.GetAccess().GetKind()
		if access == "" {
			return fmt.Errorf("accepted network instance for %s has no access kind", destination)
		}
		if views[access] != nil {
			return fmt.Errorf("accepted network mapping for %s has two %s instances", destination, access)
		}
		if instance.GetHostname() == "" || instance.GetHost() == "" || instance.GetAddress() == "" || instance.GetPort() == 0 {
			return fmt.Errorf("accepted %s network instance for %s is incomplete: hostname=%q host=%q address=%q port=%d",
				access, destination, instance.GetHostname(), instance.GetHost(), instance.GetAddress(), instance.GetPort())
		}
		views[access] = instance
	}

	proposedViews := make(map[string]*basev0.NetworkInstance, len(proposed.GetInstances()))
	for _, instance := range proposed.GetInstances() {
		access := instance.GetAccess().GetKind()
		proposedViews[access] = instance
		if views[access] == nil {
			return fmt.Errorf("accepted network mapping for %s has no %s instance", destination, access)
		}
	}
	for access := range views {
		if proposedViews[access] == nil {
			return fmt.Errorf("accepted network mapping for %s adds an unproposed %s instance", destination, access)
		}
	}

	native, container := resources.NetworkAccessNative, resources.NetworkAccessContainer
	if proposedViews[native] != nil && proposedViews[container] != nil &&
		proposedViews[native].GetHostname() != proposedViews[container].GetHostname() &&
		views[native].GetHostname() == views[container].GetHostname() {
		return fmt.Errorf("accepted network mapping for %s reuses the host address %s from inside the container",
			destination, views[native].GetHostname())
	}
	return nil
}

// cloneNetworkMappings hands the caller a set no one else holds a pointer into:
// the agent response is owned by the gRPC layer, and the runner's own view must
// not alias the one published to every consumer through the shared state.
func cloneNetworkMappings(mappings []*basev0.NetworkMapping) []*basev0.NetworkMapping {
	if mappings == nil {
		return nil
	}
	out := make([]*basev0.NetworkMapping, 0, len(mappings))
	for _, mapping := range mappings {
		clone := &basev0.NetworkMapping{}
		proto.Merge(clone, mapping)
		out = append(out, clone)
	}
	return out
}
