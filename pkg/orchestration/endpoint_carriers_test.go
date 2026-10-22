package orchestration

import (
	"context"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// The carriers a render records for a component are the in-cluster addresses
// of its own endpoints the host routes, keyed as the gateway reads them, and
// nothing of another component's; the host's own module has none.
func TestEndpointCarriersAreTheComponentsOwnRoutes(t *testing.T) {
	ctx := context.Background()
	world, _ := hostRoutesWorld(t, SnapshotMode, nil)
	flow := &Flow{world: world}
	wiki := resources.EndpointAsEnvironmentVariableKey(&resources.EndpointInformation{Module: "wiki", Service: "backend", Name: "http", API: "http"})
	docs := resources.EndpointAsEnvironmentVariableKey(&resources.EndpointInformation{Module: "docs", Service: "documents", Name: "web", API: "http"})

	carriers, err := flow.EndpointCarriers(ctx, "wiki", resources.NewContainerNetworkAccess())
	require.NoError(t, err)
	require.Equal(t, []string{wiki}, keysOf(carriers), "the solution's backend, and nothing routed for another component")
	require.Contains(t, carriers[wiki], "wiki", "its own in-cluster address: %s", carriers[wiki])

	carriers, err = flow.EndpointCarriers(ctx, "docs", resources.NewContainerNetworkAccess())
	require.NoError(t, err)
	require.Equal(t, []string{docs}, keysOf(carriers))

	carriers, err = flow.EndpointCarriers(ctx, "host", resources.NewContainerNetworkAccess())
	require.NoError(t, err)
	require.Empty(t, carriers, "the host's own endpoint is not a route")
}

func keysOf(carriers map[string]string) []string {
	var keys []string
	for key := range carriers {
		keys = append(keys, key)
	}
	return keys
}
