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
	// The hand-out is judged with the composition since core v0.14.0, so the
	// state holds the workspace carrying both modules and the published
	// endpoints carry their declaration: internal, which permits "mind".
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml":            "name: scope\nlayout: modules\nmodules:\n    - name: mind\n    - name: users\n",
		"modules/mind/module.codefly.yaml":  "kind: module\nname: mind\n",
		"modules/users/module.codefly.yaml": "kind: module\nname: users\n",
	})
	published := func(name, api string) *basev0.Endpoint {
		return &basev0.Endpoint{Module: "users", Service: "accounts", Name: name, Api: api, Visibility: resources.VisibilityInternal}
	}
	state := &StateManager{
		workspace: workspace,
		endpoints: map[string][]*basev0.Endpoint{
			dependencyUnique: {
				published("connect", standards.CONNECT),
				published("grpc", standards.GRPC),
				published("rest", standards.REST),
			},
		},
		networkMappings: map[string][]*basev0.NetworkMapping{
			dependencyUnique: {
				{Endpoint: published("connect", standards.CONNECT)},
				{Endpoint: published("grpc", standards.GRPC)},
				{Endpoint: published("rest", standards.REST)},
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
