package orchestration

import (
	"context"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"github.com/stretchr/testify/require"
)

func TestDependencyRuntimeCapabilitiesHonorEndpointScope(t *testing.T) {
	const dependencyUnique = "users/accounts"
	state := &StateManager{
		endpoints: map[string][]*basev0.Endpoint{
			dependencyUnique: {
				{Name: "connect", Api: standards.CONNECT},
				{Name: "grpc", Api: standards.GRPC},
				{Name: "rest", Api: standards.REST},
			},
		},
		networkMappings: map[string][]*basev0.NetworkMapping{
			dependencyUnique: {
				{Endpoint: &basev0.Endpoint{Name: "connect", Api: standards.CONNECT}},
				{Endpoint: &basev0.Endpoint{Name: "grpc", Api: standards.GRPC}},
				{Endpoint: &basev0.Endpoint{Name: "rest", Api: standards.REST}},
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
		{
			// A capability declared by API alone selects the endpoint serving
			// it. Readiness gates on this same selector, so what a consumer is
			// handed and what the run waits for can never disagree.
			name:       "capability declared by API",
			references: []*resources.EndpointReference{{API: standards.GRPC}},
			want:       []string{"grpc"},
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
