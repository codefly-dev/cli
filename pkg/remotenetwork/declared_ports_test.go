package remotenetwork

import (
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func serviceWithSpec(spec map[string]any) *resources.Service {
	return &resources.Service{Name: "accounts", Spec: spec}
}

func TestDeclaredEndpointPortsReadsTheDeploymentSpec(t *testing.T) {
	ports, err := DeclaredEndpointPorts(serviceWithSpec(map[string]any{
		"deployment": map[string]any{"endpoint-ports": map[string]any{"grpc": 9090, "rest": uint64(8080), "mcp": float64(8082)}},
	}))
	require.NoError(t, err)
	require.Equal(t, map[string]uint16{"grpc": 9090, "rest": 8080, "mcp": 8082}, ports)

	for name, spec := range map[string]map[string]any{
		"no spec":           nil,
		"no deployment":     {"other": true},
		"no endpoint-ports": {"deployment": map[string]any{"public-egress-ports": []any{443}}},
	} {
		ports, err = DeclaredEndpointPorts(serviceWithSpec(spec))
		require.NoError(t, err, name)
		require.Nil(t, ports, name)
	}
}

func TestDeclaredEndpointPortsRefusesMalformedDeclarations(t *testing.T) {
	for name, spec := range map[string]map[string]any{
		"deployment not a map":     {"deployment": "yes"},
		"endpoint-ports not a map": {"deployment": map[string]any{"endpoint-ports": []any{9090}}},
		"string port":              {"deployment": map[string]any{"endpoint-ports": map[string]any{"grpc": "9090"}}},
		"zero port":                {"deployment": map[string]any{"endpoint-ports": map[string]any{"grpc": 0}}},
		"port out of range":        {"deployment": map[string]any{"endpoint-ports": map[string]any{"grpc": 70000}}},
		"fractional port":          {"deployment": map[string]any{"endpoint-ports": map[string]any{"grpc": 90.5}}},
	} {
		_, err := DeclaredEndpointPorts(serviceWithSpec(spec))
		require.Error(t, err, name)
	}
}

func TestCheckDeclaredEndpointPorts(t *testing.T) {
	allocated := map[string]uint16{"authority": 52893, "grpc": 9090}
	spec := func(ports map[string]any) *resources.Service {
		return serviceWithSpec(map[string]any{"deployment": map[string]any{"endpoint-ports": ports}})
	}

	require.NoError(t, CheckDeclaredEndpointPorts(serviceWithSpec(nil), allocated), "no declaration passes")
	require.NoError(t, CheckDeclaredEndpointPorts(spec(map[string]any{"authority": 52893, "grpc": 9090}), allocated), "a matching declaration passes")
	require.NoError(t, CheckDeclaredEndpointPorts(spec(map[string]any{"grpc": 9090}), allocated), "a partial matching declaration passes")

	err := CheckDeclaredEndpointPorts(spec(map[string]any{"authority": 9091, "grpc": 9090, "gone": 1234}), allocated)
	require.ErrorContains(t, err, "service accounts endpoint authority: declared port 9091 (spec.deployment.endpoint-ports) differs from the allocated in-cluster port 52893")
	require.ErrorContains(t, err, `service accounts declares spec.deployment.endpoint-ports.gone = 1234, but the render places no in-cluster endpoint "gone"`)
	require.NotContains(t, err.Error(), "endpoint grpc:")
}
