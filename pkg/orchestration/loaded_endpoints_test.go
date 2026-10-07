package orchestration

import (
	"context"
	"net"
	"testing"

	agentservices "github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func declaredFrontend(visibility, exposure string) *resources.Service {
	service := &resources.Service{Name: "frontend", Endpoints: []*resources.Endpoint{{
		Name: "http", API: "http", Visibility: visibility, Exposure: exposure,
	}}}
	service.WithModule("saas")
	return service
}

func TestLoadedEndpointsUseTheManifestDeclaration(t *testing.T) {
	for _, test := range []struct {
		name, visibility, exposure, wireVisibility, wireExposure string
	}{
		{"older agent with exposed endpoint", "public", "public", "public", ""},
		{"older agent with explicitly unexposed endpoint", "public", "none", "public", ""},
		{"agent cannot expose unexposed endpoint", "public", "none", "public", "public"},
		{"agent cannot export private endpoint", "private", "", "public", "public"},
		{"module interface reach", "internal", "", "private", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			reported := &basev0.Endpoint{Module: "saas", Service: "frontend", Name: "http", Api: "http",
				Visibility: test.wireVisibility, Exposure: test.wireExposure,
				Description: "discovered API", ApiDetails: resources.ToHTTPAPI(&basev0.HttpAPI{Secured: true}),
			}
			before := proto.Clone(reported)
			accepted, err := reconcileLoadedEndpoints(declaredFrontend(test.visibility, test.exposure), []*basev0.Endpoint{reported})
			require.NoError(t, err)
			require.Len(t, accepted, 1)
			require.Equal(t, test.visibility, accepted[0].Visibility)
			require.Equal(t, test.exposure, accepted[0].Exposure)
			require.Equal(t, reported.Api, accepted[0].Api)
			require.Equal(t, reported.Description, accepted[0].Description)
			require.True(t, proto.Equal(reported.ApiDetails, accepted[0].ApiDetails))
			require.True(t, proto.Equal(before, reported), "the RPC response must remain unchanged")
			require.NotSame(t, reported.ApiDetails, accepted[0].ApiDetails)
		})
	}
}

func TestLoadedEndpointsDoNotDefaultAnAbsentManifestExposure(t *testing.T) {
	_, err := reconcileLoadedEndpoints(declaredFrontend("public", ""), []*basev0.Endpoint{{
		Module: "saas", Service: "frontend", Name: "http", Api: "http", Visibility: "public", Exposure: "public",
	}})
	require.ErrorContains(t, err, "states no exposure")
}

func TestLoadedEndpointsUseTheManifestLocation(t *testing.T) {
	service := declaredFrontend("private", "")
	service.Endpoints[0].Location = resources.LocationExternal
	accepted, err := reconcileLoadedEndpoints(service, []*basev0.Endpoint{{Module: "saas", Service: "frontend", Name: "http", Api: "http"}})
	require.NoError(t, err)
	require.Equal(t, resources.LocationExternal, accepted[0].Location)
}

func TestLoadedEndpointsRefuseUnownedOrMalformedResponses(t *testing.T) {
	base := &basev0.Endpoint{Module: "saas", Service: "frontend", Name: "http", Api: "http"}
	unknownWire := proto.Clone(base).(*basev0.Endpoint)
	unknownWire.ProtoReflect().SetUnknown(protowire.AppendString(protowire.AppendTag(nil, 9, protowire.BytesType), "*"))
	for _, test := range []struct {
		name     string
		reported []*basev0.Endpoint
		message  string
	}{
		{"nil endpoint", []*basev0.Endpoint{nil}, "nil endpoint"},
		{"undeclared endpoint", []*basev0.Endpoint{{Module: "saas", Service: "frontend", Name: "admin"}}, "not declared"},
		{"other module", []*basev0.Endpoint{{Module: "other", Service: "frontend", Name: "http"}}, "does not belong"},
		{"other service", []*basev0.Endpoint{{Module: "saas", Service: "other", Name: "http"}}, "does not belong"},
		{"different API", []*basev0.Endpoint{{Module: "saas", Service: "frontend", Name: "http", Api: "grpc"}}, "reports API"},
		{"missing API", []*basev0.Endpoint{{Module: "saas", Service: "frontend", Name: "http"}}, "reports API"},
		{"duplicate endpoint", []*basev0.Endpoint{base, base}, "reported twice"},
		{"unknown wire declaration", []*basev0.Endpoint{unknownWire}, "allow"},
	} {
		t.Run(test.name, func(t *testing.T) {
			accepted, err := reconcileLoadedEndpoints(declaredFrontend("public", "public"), test.reported)
			require.ErrorContains(t, err, test.message)
			require.Nil(t, accepted)
		})
	}
}

// This peer behaves like a released pre-exposure agent at the real Runtime
// boundary: Load reports no exposure, and Init accepts the host's proposal.
type endpointLoadPeer struct {
	realAgent
	endpoints []*basev0.Endpoint
}

func (peer *endpointLoadPeer) Load(context.Context, *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	return &runtimev0.LoadResponse{Status: &runtimev0.LoadStatus{State: runtimev0.LoadStatus_READY}, Endpoints: peer.endpoints}, nil
}

func loadedEndpointRunner(t *testing.T) (*endpointLoadPeer, *Runner, *World) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	peer := &endpointLoadPeer{realAgent: realAgent{mode: realAgentEcho}}
	server := grpc.NewServer()
	runtimev0.RegisterRuntimeServer(server, peer)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	runner, world := gatewayRunner(t, agentservices.NewRuntimeAgentClient(conn))
	return peer, runner, world
}

func TestRunnerLoadAndInitPreserveAnOlderAgentsDeclaredExposure(t *testing.T) {
	peer, runner, world := loadedEndpointRunner(t)
	for _, endpoint := range runner.endpoints {
		older := proto.Clone(endpoint).(*basev0.Endpoint)
		older.Exposure = ""
		peer.endpoints = append(peer.endpoints, older)
	}
	runner.endpoints = nil
	_, err := runner.Load(context.Background())
	require.NoError(t, err)
	for _, endpoint := range runner.endpoints {
		require.Equal(t, resources.ExposurePublic, endpoint.Exposure)
	}
	require.Equal(t, runner.endpoints, world.SharedState.RecordedEndpoints("web/gateway"))
	_, err = runner.Init(context.Background())
	require.NoError(t, err, "the network allocator must see the manifest's explicit exposure")
	for _, mapping := range runner.networkMappings {
		require.Equal(t, resources.ExposurePublic, mapping.Endpoint.Exposure)
		require.NotNil(t, resources.FilterNetworkInstance(context.Background(), mapping.Instances, resources.NewPublicNetworkAccess()))
	}
}

func TestRunnerLoadRefusesAnUndeclaredEndpointWithoutPublishing(t *testing.T) {
	peer, runner, world := loadedEndpointRunner(t)
	before := runner.endpoints
	peer.endpoints = []*basev0.Endpoint{{Module: "web", Service: "gateway", Name: "admin", Api: "rest"}}
	_, err := runner.Load(context.Background())
	require.ErrorContains(t, err, "not declared by the producer manifest")
	require.Equal(t, before, runner.endpoints)
	require.Equal(t, before, world.SharedState.RecordedEndpoints("web/gateway"))
	require.Nil(t, runner.outputPropertyForLoad.processed)
}
