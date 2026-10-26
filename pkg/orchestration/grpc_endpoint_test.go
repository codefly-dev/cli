package orchestration

import (
	"context"
	"testing"

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
	})
	ctx := context.Background()
	target, err := flow.GRPCEndpointTarget(ctx, "platform", "host", "delivery")
	require.NoError(t, err)
	require.Equal(t, GRPCTarget{Address: "localhost:7001"}, target)
	target, err = flow.GRPCEndpointTarget(ctx, "platform", "host", "secured")
	require.NoError(t, err)
	require.Equal(t, GRPCTarget{Address: "localhost:7002", Secure: true}, target)
	_, err = flow.GRPCEndpointTarget(ctx, "platform", "host", "rest")
	require.ErrorContains(t, err, "not gRPC")
	_, err = flow.GRPCEndpointTarget(ctx, "platform", "host", "missing")
	require.ErrorContains(t, err, "declares no endpoint missing")
	_, err = flow.GRPCEndpointTarget(ctx, "platform", "other", "delivery")
	require.ErrorContains(t, err, "no network mapping")
	conn, err := target.Dial()
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}
