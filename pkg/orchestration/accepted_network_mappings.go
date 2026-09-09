package orchestration

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"google.golang.org/protobuf/proto"
)

// canonicalAccessOrder mirrors the order network.RuntimeManager emits instances
// in. The accepted set is re-ordered onto it so two Inits that accept the same
// addresses hash the same (resources.NetworkMappingHash is order-sensitive)
// regardless of the order the agent happened to list them in.
var canonicalAccessOrder = []string{
	resources.NetworkAccessContainer,
	resources.NetworkAccessNative,
	resources.NetworkAccessPublic,
}

// acceptNetworkMappings turns an agent's Init response into the one set of
// network mappings the CLI publishes: the runner, the shared state consumers
// read (dependents' Start requests, readiness, the dashboard) and the exported
// environment all take this value.
//
// Everything is judged against the proposal rather than in the absolute, because
// the proposal is what the CLI itself generated and handed to the agent: an
// address the agent echoed back unchanged is by definition acceptable, whatever
// shape it has. Only what the agent added or altered has to stand on its own —
// it must name the endpoints that were proposed, keep one address per access
// view, not hand a private endpoint a public address, and not make an address
// less usable than the one it replaced.
//
// An empty response is the legacy path: InitResponse.network_mappings postdates
// the first agents, so an agent that returns none is taken to accept the
// proposal, which is then adopted whole. A partial response is never merged with
// the proposal endpoint by endpoint.
func acceptNetworkMappings(ctx context.Context, identity *resources.ServiceIdentity, proposed, returned []*basev0.NetworkMapping) ([]*basev0.NetworkMapping, error) {
	w := wool.Get(ctx).In("orchestration.acceptNetworkMappings", wool.ThisField(identity))
	if len(returned) == 0 {
		if len(proposed) > 0 {
			// Silence here would republish the proposal as if the agent had
			// confirmed it, which is indistinguishable from an agent that bound
			// its own ports and forgot to say so. Name the addresses being
			// assumed so that agent's author can see them.
			w.Warn(fmt.Sprintf(
				"%s returned no network mappings: assuming it serves the proposed addresses %s — an agent that binds its own ports must report them in InitResponse.network_mappings",
				identity.Unique(), resources.MakeManyNetworkMappingSummary(proposed)))
		}
		return cloneNetworkMappings(proposed), nil
	}

	proposals := make(map[string]*basev0.NetworkMapping, len(proposed))
	for _, proposal := range proposed {
		destination, err := proposedDestination(proposal)
		if err != nil {
			return nil, w.Wrap(err)
		}
		proposals[destination] = proposal
	}

	// Membership in the proposal is what establishes ownership: the proposal is
	// generated from this runner's own endpoints, so an endpoint found here was
	// proposed for this service and could not have been claimed from another.
	answers := make(map[string]*basev0.NetworkMapping, len(returned))
	for _, mapping := range returned {
		endpoint := mapping.GetEndpoint()
		if endpoint == nil {
			return nil, w.NewError("accepted network mapping has no endpoint")
		}
		destination := resources.EndpointDestination(endpoint)
		if _, ok := proposals[destination]; !ok {
			return nil, w.NewError("accepted network mapping for %s was never proposed to %s", destination, identity.Unique())
		}
		if _, ok := answers[destination]; ok {
			return nil, w.NewError("accepted network mappings contain %s twice", destination)
		}
		answers[destination] = mapping
	}

	// Walking the proposal (not the response) fixes the order of the accepted
	// set, so an agent that reorders endpoints between Inits cannot move the
	// mapping hash and trigger a propagation to every dependent.
	accepted := make([]*basev0.NetworkMapping, 0, len(proposed))
	for _, proposal := range proposed {
		endpoint := proposal.GetEndpoint()
		destination := resources.EndpointDestination(endpoint)
		mapping, ok := answers[destination]
		if !ok {
			return nil, w.NewError("accepted network mappings omit proposed endpoint %s", destination)
		}
		instances, err := acceptNetworkInstances(destination, endpoint, proposal, mapping)
		if err != nil {
			return nil, w.Wrap(err)
		}
		// The endpoint comes from the proposal so the published mapping stays
		// identical to the endpoint recorded at Load: an agent cannot restate
		// its visibility or location through the mapping it returns.
		accepted = append(accepted, &basev0.NetworkMapping{Endpoint: endpoint, Instances: instances})
	}
	return cloneNetworkMappings(accepted), nil
}

// acceptNetworkInstances returns the addresses accepted for one endpoint, in
// canonical access order. An agent may drop a view it cannot serve and may add
// one the proposal did not carry, but it may not answer the same view twice,
// give a private endpoint a public address, leave the endpoint with no address
// at all, or degrade an address it chose to rewrite.
func acceptNetworkInstances(destination string, endpoint *basev0.Endpoint, proposed, returned *basev0.NetworkMapping) ([]*basev0.NetworkInstance, error) {
	proposedViews := make(map[string]*basev0.NetworkInstance, len(proposed.GetInstances()))
	for _, instance := range proposed.GetInstances() {
		proposedViews[instance.GetAccess().GetKind()] = instance
	}

	views := make(map[string]*basev0.NetworkInstance, len(returned.GetInstances()))
	for _, instance := range returned.GetInstances() {
		access := instance.GetAccess().GetKind()
		if access == "" {
			return nil, fmt.Errorf("accepted network instance for %s has no access kind", destination)
		}
		if _, ok := views[access]; ok {
			return nil, fmt.Errorf("accepted network mapping for %s has two %s instances", destination, access)
		}
		if access == resources.NetworkAccessPublic && !publiclyReachable(endpoint) {
			return nil, fmt.Errorf("accepted network mapping for %s adds a public address to an endpoint whose visibility is %q",
				destination, endpoint.GetVisibility())
		}
		if err := acceptNetworkInstance(destination, access, proposedViews[access], instance); err != nil {
			return nil, err
		}
		views[access] = instance
	}
	if len(views) == 0 {
		return nil, fmt.Errorf("accepted network mapping for %s carries no address", destination)
	}

	out := make([]*basev0.NetworkInstance, 0, len(views))
	for _, access := range canonicalAccessOrder {
		if instance, ok := views[access]; ok {
			out = append(out, instance)
			delete(views, access)
		}
	}
	for _, access := range slices.Sorted(maps.Keys(views)) {
		out = append(out, views[access])
	}
	return out, nil
}

// acceptNetworkInstance judges one address. A field the agent left exactly as
// proposed is never judged: the CLI generated the proposal, so failing an agent
// for echoing it back would reject the CLI's own output — an external endpoint
// whose DNS record carries no port proposes port 0, and an agent that faithfully
// returns it is not the one at fault.
func acceptNetworkInstance(destination, access string, proposed, accepted *basev0.NetworkInstance) error {
	// A view the proposal never carried has no baseline to be echoed from, so
	// every field of it is the agent's own and every field must hold up.
	invented := proposed == nil
	for _, field := range []struct{ name, was, now string }{
		{"hostname", proposed.GetHostname(), accepted.GetHostname()},
		{"host", proposed.GetHost(), accepted.GetHost()},
		{"address", proposed.GetAddress(), accepted.GetAddress()},
	} {
		if (invented || field.now != field.was) && field.now == "" {
			return fmt.Errorf("accepted %s network instance for %s has no %s", access, destination, field.name)
		}
	}
	if (invented || accepted.GetPort() != proposed.GetPort()) && accepted.GetPort() == 0 {
		return fmt.Errorf("accepted %s network instance for %s has no port", access, destination)
	}
	// A container consumer resolves loopback to its own namespace, so an agent
	// that rewrites the container address onto the host one silently points
	// every containerized dependent at itself. An address left as proposed is
	// exempt: the CLI's own DNS-backed mappings share one host across views.
	if access == resources.NetworkAccessContainer &&
		accepted.GetHostname() != proposed.GetHostname() &&
		isLoopbackHostname(accepted.GetHostname()) {
		return fmt.Errorf("accepted container network instance for %s is the loopback address %s, which resolves inside the container rather than to the host",
			destination, accepted.GetHostname())
	}
	return nil
}

// publiclyReachable mirrors the condition network.RuntimeManager uses to emit a
// public instance, so an agent may answer with one exactly where the CLI would
// have proposed one.
func publiclyReachable(endpoint *basev0.Endpoint) bool {
	return endpoint.GetVisibility() == resources.VisibilityPublic || resources.IsExternalEndpoint(endpoint)
}

func isLoopbackHostname(hostname string) bool {
	switch strings.ToLower(hostname) {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return false
}

// proposedDestination guards the proposal side of the identity lookup, which
// resources.EndpointDestination cannot do itself: it dereferences the endpoint.
func proposedDestination(proposal *basev0.NetworkMapping) (string, error) {
	endpoint := proposal.GetEndpoint()
	if endpoint == nil {
		return "", fmt.Errorf("proposed network mapping has no endpoint")
	}
	return resources.EndpointDestination(endpoint), nil
}

// cloneNetworkMappings detaches the accepted set from the objects it was built
// out of: the agent's gRPC response, and the endpoints the runner recorded at
// Load.
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
