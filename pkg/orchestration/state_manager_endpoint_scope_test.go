package orchestration

import (
	"context"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestDependencyRuntimeCapabilitiesHonorEndpointScope(t *testing.T) {
	const dependencyUnique = "users/accounts"
	state := &StateManager{
		endpoints: map[string][]*basev0.Endpoint{
			dependencyUnique: {
				{Name: "connect"},
				{Name: "grpc"},
				{Name: "rest"},
			},
		},
		networkMappings: map[string][]*basev0.NetworkMapping{
			dependencyUnique: {
				{Endpoint: &basev0.Endpoint{Name: "connect"}},
				{Endpoint: &basev0.Endpoint{Name: "grpc"}},
				{Endpoint: &basev0.Endpoint{Name: "rest"}},
			},
		},
	}

	tests := []struct {
		name       string
		references []*resources.EndpointReference
		want       []string
	}{
		{
			name:       "explicit capability",
			references: []*resources.EndpointReference{{Name: "grpc"}},
			want:       []string{"grpc"},
		},
		{
			name: "unspecified capabilities preserve all endpoints",
			want: []string{"connect", "grpc", "rest"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			consumer := &resources.Service{
				Name: "mind",
				ServiceDependencies: []*resources.ServiceDependency{
					{Name: "accounts", Module: "users", Endpoints: test.references},
				},
			}
			consumer.WithModule("mind")

			endpoints, err := state.GetDependenciesEndpoints(context.Background(), consumer)
			require.NoError(t, err)
			require.Equal(t, test.want, endpointNames(endpoints))

			mappings, err := state.GetDependenciesNetworkMappings(context.Background(), consumer)
			require.NoError(t, err)
			require.Equal(t, test.want, mappingEndpointNames(mappings))
		})
	}
}

func endpointNames(endpoints []*basev0.Endpoint) []string {
	names := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		names = append(names, endpoint.GetName())
	}
	return names
}

func mappingEndpointNames(mappings []*basev0.NetworkMapping) []string {
	names := make([]string, 0, len(mappings))
	for _, mapping := range mappings {
		names = append(names, mapping.GetEndpoint().GetName())
	}
	return names
}
