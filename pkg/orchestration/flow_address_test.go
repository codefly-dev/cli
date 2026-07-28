package orchestration

import (
	"context"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestGetAddressForEndpointPrefersHostNativeMapping(t *testing.T) {
	flow := flowWithNetworkMappings("web/frontend", []*basev0.NetworkMapping{
		{
			Endpoint: &basev0.Endpoint{Name: "http"},
			Instances: []*basev0.NetworkInstance{
				networkInstance(resources.NetworkAccessContainer, "http://host.docker.internal:3000"),
				networkInstance(resources.NetworkAccessPublic, "https://example.test"),
				networkInstance(resources.NetworkAccessNative, "http://localhost:53231"),
			},
		},
	})

	address, err := flow.GetAddressForEndpoint(context.Background(), "web", "frontend", "http")
	require.NoError(t, err)
	require.Equal(t, "http://localhost:53231", address)
}

func TestGetAddressForEndpointFallsBackToPublicMapping(t *testing.T) {
	flow := flowWithNetworkMappings("web/frontend", []*basev0.NetworkMapping{
		{
			Endpoint: &basev0.Endpoint{Name: "http"},
			Instances: []*basev0.NetworkInstance{
				networkInstance(resources.NetworkAccessContainer, "http://frontend:3000"),
				networkInstance(resources.NetworkAccessPublic, "https://example.test"),
			},
		},
	})

	address, err := flow.GetAddressForEndpoint(context.Background(), "web", "frontend", "http")
	require.NoError(t, err)
	require.Equal(t, "https://example.test", address)
}

func TestGetAddressForEndpointRejectsContainerOnlyMapping(t *testing.T) {
	flow := flowWithNetworkMappings("web/frontend", []*basev0.NetworkMapping{
		{
			Endpoint: &basev0.Endpoint{Name: "http"},
			Instances: []*basev0.NetworkInstance{
				networkInstance(resources.NetworkAccessContainer, "http://frontend:3000"),
			},
		},
	})

	_, err := flow.GetAddressForEndpoint(context.Background(), "web", "frontend", "http")
	require.ErrorContains(t, err, "cannot find network mappings for web/frontend")
}

func flowWithNetworkMappings(unique string, mappings []*basev0.NetworkMapping) *Flow {
	return &Flow{
		SharedState: &StateManager{
			networkMappings: map[string][]*basev0.NetworkMapping{unique: mappings},
		},
	}
}

func networkInstance(kind, address string) *basev0.NetworkInstance {
	return &basev0.NetworkInstance{
		Access:  &basev0.NetworkAccess{Kind: kind},
		Address: address,
	}
}
