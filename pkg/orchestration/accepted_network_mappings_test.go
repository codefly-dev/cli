package orchestration

import (
	"context"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func gatewayIdentity() *resources.ServiceIdentity {
	return &resources.ServiceIdentity{Module: "web", Name: "gateway"}
}

func gatewayEndpoint(name string) *basev0.Endpoint {
	return &basev0.Endpoint{Module: "web", Service: "gateway", Name: name, Api: name}
}

func publicGatewayEndpoint(name string) *basev0.Endpoint {
	endpoint := gatewayEndpoint(name)
	endpoint.Visibility = resources.VisibilityPublic
	return endpoint
}

// proposedMapping mirrors what network.RuntimeManager hands the agent: one
// container view and one native view per endpoint, on the same port.
func proposedMapping(name string, port uint16) *basev0.NetworkMapping {
	endpoint := gatewayEndpoint(name)
	return &basev0.NetworkMapping{
		Endpoint: endpoint,
		Instances: []*basev0.NetworkInstance{
			network.Container(endpoint, port),
			network.Native(endpoint, port),
		},
	}
}

func acceptedPortOf(t *testing.T, mappings []*basev0.NetworkMapping, name, access string) uint32 {
	t.Helper()
	for _, mapping := range mappings {
		if mapping.GetEndpoint().GetName() != name {
			continue
		}
		for _, instance := range mapping.Instances {
			if instance.GetAccess().GetKind() == access {
				return instance.Port
			}
		}
	}
	t.Fatalf("no %s instance for endpoint %s", access, name)
	return 0
}

func TestAcceptNetworkMappingsTakesTheAgentPort(t *testing.T) {
	proposed := []*basev0.NetworkMapping{proposedMapping("grpc", 9000)}
	returned := []*basev0.NetworkMapping{proposedMapping("grpc", 41337)}

	accepted, err := acceptNetworkMappings(context.Background(), gatewayIdentity(), proposed, returned)
	require.NoError(t, err)
	require.Equal(t, uint32(41337), acceptedPortOf(t, accepted, "grpc", resources.NetworkAccessNative))
	require.Equal(t, uint32(41337), acceptedPortOf(t, accepted, "grpc", resources.NetworkAccessContainer))
}

// The accepted set must not alias the response the agent sent nor the proposal:
// both the runner and every shared-state consumer read from it.
func TestAcceptNetworkMappingsOwnsItsResult(t *testing.T) {
	proposed := []*basev0.NetworkMapping{proposedMapping("grpc", 9000)}
	returned := []*basev0.NetworkMapping{proposedMapping("grpc", 41337)}

	accepted, err := acceptNetworkMappings(context.Background(), gatewayIdentity(), proposed, returned)
	require.NoError(t, err)

	for _, instance := range returned[0].Instances {
		instance.Port = 1
		instance.Host = "mutated:1"
	}
	require.Equal(t, uint32(41337), acceptedPortOf(t, accepted, "grpc", resources.NetworkAccessNative))
}

// An agent that predates InitResponse.network_mappings returns none; the
// proposal is then the accepted set, adopted whole and owned by the caller.
func TestAcceptNetworkMappingsAdoptsProposalWhenAgentReturnsNone(t *testing.T) {
	proposed := []*basev0.NetworkMapping{proposedMapping("grpc", 9000)}

	accepted, err := acceptNetworkMappings(context.Background(), gatewayIdentity(), proposed, nil)
	require.NoError(t, err)
	require.Equal(t, uint32(9000), acceptedPortOf(t, accepted, "grpc", resources.NetworkAccessNative))

	proposed[0].Instances[0].Port = 1
	require.Equal(t, uint32(9000), acceptedPortOf(t, accepted, "grpc", resources.NetworkAccessContainer))
}

func TestAcceptNetworkMappingsRejectsInvalidResponses(t *testing.T) {
	foreign := proposedMapping("grpc", 9000)
	foreign.Endpoint = &basev0.Endpoint{Module: "billing", Service: "accounts", Name: "grpc", Api: "grpc"}

	noAccess := proposedMapping("grpc", 9000)
	noAccess.Instances[0].Access = nil

	incomplete := proposedMapping("grpc", 9000)
	incomplete.Instances[1].Port = 0

	duplicateView := proposedMapping("grpc", 9000)
	duplicateView.Instances = append(duplicateView.Instances, network.Native(gatewayEndpoint("grpc"), 9001))

	noView := proposedMapping("grpc", 9000)
	noView.Instances = nil

	publicOnPrivate := proposedMapping("grpc", 9000)
	publicOnPrivate.Instances = append(publicOnPrivate.Instances, network.PublicDefault(gatewayEndpoint("grpc"), 9000))

	containerOnHost := proposedMapping("grpc", 41337)
	containerOnHost.Instances[0] = network.Native(gatewayEndpoint("grpc"), 41337)
	containerOnHost.Instances[0].Access = resources.NewContainerNetworkAccess()

	// A view the proposal never carried has no baseline, so it must stand on
	// its own — unlike an echoed one, which is judged against the proposal.
	incompleteNewView := proposedMapping("grpc", 9000)
	incompleteNewView.Instances[0].Port = 0

	for _, test := range []struct {
		name     string
		proposed []*basev0.NetworkMapping
		returned []*basev0.NetworkMapping
		message  string
	}{
		{
			name:     "no endpoint",
			proposed: []*basev0.NetworkMapping{proposedMapping("grpc", 9000)},
			returned: []*basev0.NetworkMapping{{Instances: proposedMapping("grpc", 41337).Instances}},
			message:  "has no endpoint",
		},
		{
			name:     "foreign endpoint",
			proposed: []*basev0.NetworkMapping{proposedMapping("grpc", 9000)},
			returned: []*basev0.NetworkMapping{foreign},
			message:  "was never proposed to web/gateway",
		},
		{
			name:     "unproposed endpoint",
			proposed: []*basev0.NetworkMapping{proposedMapping("grpc", 9000)},
			returned: []*basev0.NetworkMapping{proposedMapping("rest", 41337)},
			message:  "was never proposed",
		},
		{
			name:     "duplicate endpoint",
			proposed: []*basev0.NetworkMapping{proposedMapping("grpc", 9000)},
			returned: []*basev0.NetworkMapping{proposedMapping("grpc", 41337), proposedMapping("grpc", 41338)},
			message:  "twice",
		},
		{
			name:     "omitted endpoint",
			proposed: []*basev0.NetworkMapping{proposedMapping("grpc", 9000), proposedMapping("rest", 9001)},
			returned: []*basev0.NetworkMapping{proposedMapping("grpc", 41337)},
			message:  "omit proposed endpoint web/gateway/rest",
		},
		{
			name:     "instance without access",
			proposed: []*basev0.NetworkMapping{proposedMapping("grpc", 9000)},
			returned: []*basev0.NetworkMapping{noAccess},
			message:  "has no access kind",
		},
		{
			name:     "port cleared by the agent",
			proposed: []*basev0.NetworkMapping{proposedMapping("grpc", 9000)},
			returned: []*basev0.NetworkMapping{incomplete},
			message:  "has no port",
		},
		{
			name:     "invented view is judged on its own",
			proposed: []*basev0.NetworkMapping{{Endpoint: gatewayEndpoint("grpc"), Instances: proposedMapping("grpc", 9000).Instances[1:]}},
			returned: []*basev0.NetworkMapping{incompleteNewView},
			message:  "has no port",
		},
		{
			name:     "duplicate access view",
			proposed: []*basev0.NetworkMapping{proposedMapping("grpc", 9000)},
			returned: []*basev0.NetworkMapping{duplicateView},
			message:  "two native instances",
		},
		{
			name:     "every access view dropped",
			proposed: []*basev0.NetworkMapping{proposedMapping("grpc", 9000)},
			returned: []*basev0.NetworkMapping{noView},
			message:  "carries no address",
		},
		{
			name:     "public address on a private endpoint",
			proposed: []*basev0.NetworkMapping{proposedMapping("grpc", 9000)},
			returned: []*basev0.NetworkMapping{publicOnPrivate},
			message:  "adds a public address to an endpoint whose visibility is",
		},
		{
			name:     "container view rewritten onto loopback",
			proposed: []*basev0.NetworkMapping{proposedMapping("grpc", 9000)},
			returned: []*basev0.NetworkMapping{containerOnHost},
			message:  "is the loopback address localhost",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			accepted, err := acceptNetworkMappings(context.Background(), gatewayIdentity(), test.proposed, test.returned)
			require.ErrorContains(t, err, test.message)
			require.Nil(t, accepted)
		})
	}
}

// The CLI generates the proposal, so an address it produced must survive being
// echoed back. An external endpoint whose DNS record carries no port proposes
// port 0; failing the agent for returning it would reject the CLI's own output —
// and would make acceptance depend on whether the agent echoes at all, since the
// legacy path publishes the very same value.
func TestAcceptNetworkMappingsEchoesBackTheCLIsOwnDNSAddresses(t *testing.T) {
	endpoint := &basev0.Endpoint{Module: "web", Service: "gateway", Name: "rest", Api: "rest", Location: "external"}
	identity := gatewayIdentity()
	dnsInstances := func() []*basev0.NetworkInstance {
		dns := &basev0.DNS{Host: "api.example.com", Secured: true, Endpoint: "rest"}
		return []*basev0.NetworkInstance{
			network.ContainerInstance(network.DNS(identity, endpoint, dns)),
			network.NativeInstance(network.DNS(identity, endpoint, dns)),
		}
	}
	proposed := []*basev0.NetworkMapping{{Endpoint: endpoint, Instances: dnsInstances()}}
	echoed := []*basev0.NetworkMapping{{Endpoint: endpoint, Instances: dnsInstances()}}

	accepted, err := acceptNetworkMappings(context.Background(), identity, proposed, echoed)
	require.NoError(t, err)
	require.Equal(t, uint32(0), acceptedPortOf(t, accepted, "rest", resources.NetworkAccessContainer))

	// The legacy path must reach the same verdict on the same input.
	legacy, err := acceptNetworkMappings(context.Background(), identity, proposed, nil)
	require.NoError(t, err)
	require.Equal(t, uint32(0), acceptedPortOf(t, legacy, "rest", resources.NetworkAccessContainer))
}

// Ownership is established by membership in the proposal, which is generated
// from this runner's own endpoints. An endpoint Load attributed to another
// module is still this service's to serve, and rejecting it on the identity
// alone would fail the CLI's own proposal.
func TestAcceptNetworkMappingsAcceptsAProposedEndpointAttributedElsewhere(t *testing.T) {
	endpoint := &basev0.Endpoint{Module: "billing", Service: "accounts", Name: "grpc", Api: "grpc"}
	proposed := []*basev0.NetworkMapping{{Endpoint: endpoint, Instances: []*basev0.NetworkInstance{
		network.Container(endpoint, 9000), network.Native(endpoint, 9000)}}}
	returned := []*basev0.NetworkMapping{{Endpoint: endpoint, Instances: []*basev0.NetworkInstance{
		network.Container(endpoint, 41337), network.Native(endpoint, 41337)}}}

	accepted, err := acceptNetworkMappings(context.Background(), gatewayIdentity(), proposed, returned)
	require.NoError(t, err)
	require.Equal(t, uint32(41337), acceptedPortOf(t, accepted, "grpc", resources.NetworkAccessNative))
}

// resources.NetworkMappingHash is order-sensitive, and it drives whether Init
// propagates to every dependent. An agent that lists the same addresses in a
// different order between Inits must not move the hash.
func TestAcceptNetworkMappingsIsOrderInsensitive(t *testing.T) {
	proposed := []*basev0.NetworkMapping{proposedMapping("grpc", 9000), proposedMapping("rest", 9001)}

	forward := []*basev0.NetworkMapping{proposedMapping("grpc", 41337), proposedMapping("rest", 41338)}
	shuffled := []*basev0.NetworkMapping{proposedMapping("rest", 41338), proposedMapping("grpc", 41337)}
	shuffled[0].Instances[0], shuffled[0].Instances[1] = shuffled[0].Instances[1], shuffled[0].Instances[0]

	first, err := acceptNetworkMappings(context.Background(), gatewayIdentity(), proposed, forward)
	require.NoError(t, err)
	second, err := acceptNetworkMappings(context.Background(), gatewayIdentity(), proposed, shuffled)
	require.NoError(t, err)

	require.Equal(t, resources.NetworkMappingHash(first...), resources.NetworkMappingHash(second...),
		"endpoint or instance order from the agent moved the propagation hash")
}

// An agent may serve fewer views than proposed, and may add one the CLI would
// itself have proposed for a public endpoint.
func TestAcceptNetworkMappingsAllowsAgentChosenAccessViews(t *testing.T) {
	dropped := proposedMapping("grpc", 41337)
	dropped.Instances = dropped.Instances[1:]
	accepted, err := acceptNetworkMappings(context.Background(), gatewayIdentity(),
		[]*basev0.NetworkMapping{proposedMapping("grpc", 9000)}, []*basev0.NetworkMapping{dropped})
	require.NoError(t, err)
	require.Len(t, accepted[0].Instances, 1)
	require.Equal(t, resources.NetworkAccessNative, accepted[0].Instances[0].Access.Kind)

	endpoint := publicGatewayEndpoint("rest")
	proposal := &basev0.NetworkMapping{Endpoint: endpoint, Instances: []*basev0.NetworkInstance{
		network.Container(endpoint, 9000), network.Native(endpoint, 9000)}}
	added := &basev0.NetworkMapping{Endpoint: endpoint, Instances: []*basev0.NetworkInstance{
		network.Container(endpoint, 41337), network.Native(endpoint, 41337), network.PublicDefault(endpoint, 41337)}}
	accepted, err = acceptNetworkMappings(context.Background(), gatewayIdentity(),
		[]*basev0.NetworkMapping{proposal}, []*basev0.NetworkMapping{added})
	require.NoError(t, err)
	require.Equal(t, uint32(41337), acceptedPortOf(t, accepted, "rest", resources.NetworkAccessPublic))
}

// An agent cannot restate an endpoint's visibility through the mapping it
// returns: the published mapping keeps the endpoint recorded at Load, so a
// private endpoint cannot be re-labelled public and land in the public split.
func TestAcceptNetworkMappingsKeepsTheProposedEndpoint(t *testing.T) {
	relabelled := proposedMapping("grpc", 41337)
	relabelled.Endpoint = gatewayEndpoint("grpc")
	relabelled.Endpoint.Visibility = resources.VisibilityPublic

	accepted, err := acceptNetworkMappings(context.Background(), gatewayIdentity(),
		[]*basev0.NetworkMapping{proposedMapping("grpc", 9000)}, []*basev0.NetworkMapping{relabelled})
	require.NoError(t, err)
	require.Empty(t, accepted[0].Endpoint.Visibility)
}

// A nil endpoint on the proposal side must be an error, not a panic:
// resources.EndpointDestination dereferences the endpoint it is given.
func TestAcceptNetworkMappingsRejectsANilProposedEndpoint(t *testing.T) {
	_, err := acceptNetworkMappings(context.Background(), gatewayIdentity(),
		[]*basev0.NetworkMapping{{}}, []*basev0.NetworkMapping{proposedMapping("grpc", 41337)})
	require.ErrorContains(t, err, "proposed network mapping has no endpoint")
}
