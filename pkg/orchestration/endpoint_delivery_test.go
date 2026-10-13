package orchestration

import (
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

func TestEndpointDeliveryResolvesAfterTheHostStarts(t *testing.T) {
	world, workspace := hostRoutesWorld(t, RunMode, nil)
	flow := &Flow{world: world}
	var document *solutionhost.SolutionHostBinding
	for _, fixture := range solutionhost.FixturesOf(solutionhost.DocumentTypePresence) {
		parsed, err := solutionhost.Parse(fixture.Document)
		if err == nil && !parsed.Removed {
			document = parsed
			break
		}
	}
	require.NotNil(t, document)
	document.Endpoints = []solutionhost.Endpoint{{Module: "docs", Service: "documents", Name: "web", API: "http", Visibility: "internal"}}
	require.NoError(t, document.Validate())
	_, err := flow.EndpointDelivery(t.Context(), document, resources.NewNativeNetworkAccess())
	require.ErrorContains(t, err, "has not allocated")

	// The host has already started. Only this producer now gets an allocated
	// address; another solution's unallocated backend must not prevent delivery.
	producer := parityService(t, workspace, "docs", "documents")
	identity, err := producer.Identity()
	require.NoError(t, err)
	endpoints, err := producer.LoadEndpoints(t.Context())
	require.NoError(t, err)
	require.NoError(t, world.SharedState.RecordEndpoints(t.Context(), identity, endpoints))
	raw, err := flow.EndpointDelivery(t.Context(), document, resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	projection, err := solutionhost.ParseEndpointDelivery(raw, document)
	require.NoError(t, err)
	require.Len(t, projection.Carriers, 1)
	for _, address := range projection.Carriers {
		require.Contains(t, address, "localhost")
	}
	document.Generation++
	_, err = solutionhost.ParseEndpointDelivery(raw, document)
	require.Error(t, err)
	raw, err = flow.EndpointDelivery(t.Context(), document, resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	_, err = solutionhost.ParseEndpointDelivery(raw, document)
	require.NoError(t, err)
}
