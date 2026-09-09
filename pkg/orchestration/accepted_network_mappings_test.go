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

	missingView := proposedMapping("grpc", 9000)
	missingView.Instances = missingView.Instances[:1]

	extraView := proposedMapping("grpc", 9000)
	extraView.Instances = append(extraView.Instances, network.PublicDefault(gatewayEndpoint("grpc"), 9000))

	containerOnHost := proposedMapping("grpc", 41337)
	containerOnHost.Instances[0] = network.Native(gatewayEndpoint("grpc"), 41337)
	containerOnHost.Instances[0].Access = resources.NewContainerNetworkAccess()

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
			message:  "is not owned by web/gateway",
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
			name:     "incomplete instance",
			proposed: []*basev0.NetworkMapping{proposedMapping("grpc", 9000)},
			returned: []*basev0.NetworkMapping{incomplete},
			message:  "is incomplete",
		},
		{
			name:     "duplicate access view",
			proposed: []*basev0.NetworkMapping{proposedMapping("grpc", 9000)},
			returned: []*basev0.NetworkMapping{duplicateView},
			message:  "two native instances",
		},
		{
			name:     "dropped access view",
			proposed: []*basev0.NetworkMapping{proposedMapping("grpc", 9000)},
			returned: []*basev0.NetworkMapping{missingView},
			message:  "has no native instance",
		},
		{
			name:     "invented access view",
			proposed: []*basev0.NetworkMapping{proposedMapping("grpc", 9000)},
			returned: []*basev0.NetworkMapping{extraView},
			message:  "adds an unproposed public instance",
		},
		{
			name:     "container view on the host address",
			proposed: []*basev0.NetworkMapping{proposedMapping("grpc", 9000)},
			returned: []*basev0.NetworkMapping{containerOnHost},
			message:  "reuses the host address",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			accepted, err := acceptNetworkMappings(context.Background(), gatewayIdentity(), test.proposed, test.returned)
			require.ErrorContains(t, err, test.message)
			require.Nil(t, accepted)
		})
	}
}
