package orchestration

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"github.com/stretchr/testify/require"
)

// A running service's gRPC endpoint is reached at its host-native instance —
// whatever scheme its recorded address carries — with the transport its own
// `secured` declaration asks for; an endpoint serving another API is refused.
func TestGRPCEndpointTargetIsTheNativeInstanceWithItsDeclaredTransport(t *testing.T) {
	grpcEndpoint := func(name string, secured bool) *basev0.Endpoint {
		return &basev0.Endpoint{Name: name, Api: standards.GRPC, ApiDetails: resources.ToGrpcAPI(&basev0.GrpcAPI{Secured: secured})}
	}
	flow := flowWithNetworkMappings("platform/host", []*basev0.NetworkMapping{
		{Endpoint: grpcEndpoint("delivery", false), Instances: []*basev0.NetworkInstance{
			networkInstance(resources.NetworkAccessContainer, "grpc://host.docker.internal:7000"),
			networkInstance(resources.NetworkAccessNative, "grpc://localhost:7001"),
		}},
		{Endpoint: grpcEndpoint("secured", true), Instances: []*basev0.NetworkInstance{networkInstance(resources.NetworkAccessNative, "https://localhost:7002")}},
		{Endpoint: &basev0.Endpoint{Name: "rest", Api: standards.REST}, Instances: []*basev0.NetworkInstance{networkInstance(resources.NetworkAccessNative, "http://localhost:7003")}},
		{Endpoint: &basev0.Endpoint{Name: "connect", Api: standards.CONNECT, ApiDetails: resources.ToHTTPAPI(&basev0.HttpAPI{})}, Instances: []*basev0.NetworkInstance{networkInstance(resources.NetworkAccessNative, "http://localhost:7004")}},
		{Endpoint: &basev0.Endpoint{Name: "connect-tls", Api: standards.CONNECT, ApiDetails: resources.ToHTTPAPI(&basev0.HttpAPI{Secured: true})}, Instances: []*basev0.NetworkInstance{networkInstance(resources.NetworkAccessNative, "https://localhost:7005")}},
	})
	ctx := context.Background()
	target, err := flow.GRPCEndpointTarget(ctx, "platform", "host", "delivery")
	require.NoError(t, err)
	require.Equal(t, GRPCTarget{Address: "localhost:7001"}, target)
	target, err = flow.GRPCEndpointTarget(ctx, "platform", "host", "secured")
	require.NoError(t, err)
	require.Equal(t, GRPCTarget{Address: "localhost:7002", Secure: true}, target)
	// A Connect listener answers gRPC on the same port: a host whose
	// composition interface is its Connect face is reached there.
	connectTarget, err := flow.GRPCEndpointTarget(ctx, "platform", "host", "connect")
	require.NoError(t, err)
	require.Equal(t, GRPCTarget{Address: "localhost:7004"}, connectTarget)
	connectTarget, err = flow.GRPCEndpointTarget(ctx, "platform", "host", "connect-tls")
	require.NoError(t, err)
	require.Equal(t, GRPCTarget{Address: "localhost:7005", Secure: true}, connectTarget)
	_, err = flow.GRPCEndpointTarget(ctx, "platform", "host", "rest")
	require.ErrorContains(t, err, "speaks neither gRPC nor Connect")
	_, err = flow.GRPCEndpointTarget(ctx, "platform", "host", "missing")
	require.ErrorContains(t, err, "declares no endpoint missing")
	_, err = flow.GRPCEndpointTarget(ctx, "platform", "other", "delivery")
	require.ErrorContains(t, err, "no network mapping")
	conn, err := target.Dial()
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

// The claim the Connect case rests on, on the wire: the gRPC client the run
// delivers manifests with reaches a Connect handler of the composition
// interface on a plaintext listener, as a host serving its Connect face over
// h2c answers it.
func TestAConnectListenerAnswersTheCompositionClientOverGRPC(t *testing.T) {
	handler := connect.NewUnaryHandler(compositionv1.CompositionService_ApplyManifest_FullMethodName,
		func(_ context.Context, request *connect.Request[compositionv1.ApplyManifestRequest]) (*connect.Response[compositionv1.ApplyManifestResponse], error) {
			if request.Msg.GetCredential() != "delivery-token" {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unexpected credential"))
			}
			return connect.NewResponse(&compositionv1.ApplyManifestResponse{Generation: 7}), nil
		})
	server := httptest.NewUnstartedServer(handler)
	server.Config.Protocols = new(http.Protocols)
	server.Config.Protocols.SetHTTP1(true)
	server.Config.Protocols.SetUnencryptedHTTP2(true)
	server.Start()
	t.Cleanup(server.Close)

	flow := flowWithNetworkMappings("platform/host", []*basev0.NetworkMapping{
		{Endpoint: &basev0.Endpoint{Name: "connect", Api: standards.CONNECT, ApiDetails: resources.ToHTTPAPI(&basev0.HttpAPI{})},
			Instances: []*basev0.NetworkInstance{networkInstance(resources.NetworkAccessNative, server.URL)}},
	})
	target, err := flow.GRPCEndpointTarget(t.Context(), "platform", "host", "connect")
	require.NoError(t, err)
	conn, err := target.Dial()
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	out, err := compositionv1.NewCompositionServiceClient(conn).ApplyManifest(t.Context(), &compositionv1.ApplyManifestRequest{Credential: "delivery-token"})
	require.NoError(t, err)
	require.Equal(t, uint64(7), out.GetGeneration())
}
