package gitops

import (
	"testing"

	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

// TestPublishSignsTheAddressOfEveryRoutedEndpoint: a manifest carries the
// address of each endpoint the host routes, inside the signed bytes, so an
// address that moved is the next generation of the one document, never a
// second delivery beside it.
func TestPublishSignsTheAddressOfEveryRoutedEndpoint(t *testing.T) {
	repository := newDeliveryRepository(t)
	opts := publishOptions(&fakeSigner{})
	delivery, err := repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	require.Len(t, delivery.Documents, 1, "one manifest, no endpoint document beside it")
	require.Equal(t, []solutionhost.Endpoint{{Name: "grpc", Service: "api", API: "grpc", Address: testEndpointAddress}}, signedManifest(t, repository, "example.prod.crm").Endpoints)
	repository.deliver(t)

	moved := "http://api.crm.svc.cluster.local:9191"
	delivery, err = repository.settle(repository.stageRenderWith(t, func(options *RenderOptions) {
		options.Instance.Endpoints[0].Address = moved
	}), &opts)
	require.NoError(t, err)
	require.Equal(t, uint64(2), documentByID(delivery, "example.prod.crm").Generation)
	require.Equal(t, moved, signedManifest(t, repository, "example.prod.crm").Endpoints[0].Address)
}
