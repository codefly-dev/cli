package orchestration

import (
	"context"
	"net"
	"testing"

	agentservices "github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	coreservices "github.com/codefly-dev/core/services"
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

// builderLoadPeer behaves like a released pre-exposure agent at the Builder
// boundary: Load reports endpoints with no exposure.
type builderLoadPeer struct {
	builderv0.UnimplementedBuilderServer
	endpoints []*basev0.Endpoint
}

func (peer *builderLoadPeer) Load(context.Context, *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	return &builderv0.LoadResponse{
		State:     &builderv0.LoadStatus{State: builderv0.LoadStatus_READY},
		Endpoints: peer.endpoints,
	}, nil
}

// gatewayBuilder is gatewayRunner's Builder twin: the same workspace, module and
// service, wired to a builder agent instead of a runtime one.
func gatewayBuilder(t *testing.T, peer *builderLoadPeer) (*Builder, *World, *resources.ServiceIdentity) {
	t.Helper()
	ctx := context.Background()
	server := grpc.NewServer()
	builderv0.RegisterBuilderServer(server, peer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	workspace, err := resources.LoadWorkspaceFromDir(ctx, "testdata/module-layout")
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "web")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "gateway")
	require.NoError(t, err)
	identity, err := service.Identity()
	require.NoError(t, err)
	identity.Workspace = workspace.Name
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	configurationManager, err := configurations.NewManager(ctx, workspace)
	require.NoError(t, err)
	require.NoError(t, configurationManager.Load(ctx, env.Runtime()))
	dependencies, err := architecture.NewServiceDependencies(ctx, workspace)
	require.NoError(t, err)
	sharedState, err := NewStateManager(ctx, configurationManager, dependencies, workspace)
	require.NoError(t, err)
	world := &World{
		Env: env, Workspace: workspace, Dependencies: dependencies,
		SharedState: sharedState, ConfigurationManager: configurationManager, Mode: DeployMode,
	}
	instance := &coreservices.Instance{Workspace: workspace, Module: module, Service: service, Identity: identity}
	instance.Builder = &coreservices.BuilderInstance{
		Instance: instance,
		Builder:  agentservices.NewBuilderAgentClient(conn),
	}
	builder, err := NewBuilder(ctx, instance, world)
	require.NoError(t, err)
	return builder, world, identity
}

// The regression for the Load path the reconciliation first missed. Builder.Load
// publishes into the same shared state and the same network mappings as
// Runner.Load, so an older agent's missing exposure has to be restored there too
// — otherwise `codefly deploy` drops an outward address that `codefly run`
// allocates, for one manifest and one agent.
func TestBuilderLoadPreservesAnOlderAgentsDeclaredExposure(t *testing.T) {
	peer := &builderLoadPeer{}
	builder, world, identity := gatewayBuilder(t, peer)
	declared, err := builder.instance.Service.LoadEndpoints(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, declared)
	for _, endpoint := range declared {
		require.Equal(t, resources.ExposurePublic, endpoint.Exposure, "the fixture must declare an exposure to lose")
		older := proto.CloneOf(endpoint)
		older.Exposure = ""
		peer.endpoints = append(peer.endpoints, older)
	}

	_, err = builder.Load(context.Background())
	require.NoError(t, err)

	for _, endpoint := range builder.endpoints {
		require.Equal(t, resources.ExposurePublic, endpoint.Exposure,
			"the endpoint handed to GenerateNetworkMappings must carry the manifest's exposure")
		require.True(t, resources.IsExposedEndpoint(endpoint))
	}
	for _, endpoint := range world.SharedState.RecordedEndpoints(identity.Unique()) {
		require.Equal(t, resources.ExposurePublic, endpoint.Exposure,
			"shared state must carry the manifest's exposure")
	}
}

func TestBuilderLoadRefusesAnUndeclaredEndpoint(t *testing.T) {
	peer := &builderLoadPeer{endpoints: []*basev0.Endpoint{
		{Module: "web", Service: "gateway", Name: "admin", Api: "rest"},
	}}
	builder, world, identity := gatewayBuilder(t, peer)
	_, err := builder.Load(context.Background())
	require.ErrorContains(t, err, "not declared by the producer manifest")
	require.Nil(t, builder.endpoints)
	require.Empty(t, world.SharedState.RecordedEndpoints(identity.Unique()))
}

// An `api:` the manifest does not state is the agent's to report: core itself
// defaults it from the endpoint name when that name is a standard API, so an
// absent one is an omission the model reads as "whatever this endpoint serves".
// Refusing it stopped a local run that worked, for exactly the older agents this
// reconciliation accommodates.
func TestLoadedEndpointsAcceptTheAgentsAPIWhenTheManifestStatesNone(t *testing.T) {
	service := &resources.Service{Name: "store", Endpoints: []*resources.Endpoint{
		{Name: "write", Visibility: "private"},
	}}
	service.WithModule("data")
	require.Empty(t, service.Endpoints[0].API, "the fixture must omit the api the agent reports")

	accepted, err := reconcileLoadedEndpoints(service, []*basev0.Endpoint{
		{Module: "data", Service: "store", Name: "write", Api: "tcp"},
	})
	require.NoError(t, err)
	require.Len(t, accepted, 1)
	require.Equal(t, "tcp", accepted[0].Api, "the agent's discovered API is what the manifest left open")
	require.Equal(t, "private", accepted[0].Visibility)
}

// A manifest that DOES state an API is a declaration, and a report contradicting
// it is still refused — with a message naming what to correct.
func TestLoadedEndpointsStillRefuseAContradictedAPI(t *testing.T) {
	_, err := reconcileLoadedEndpoints(declaredFrontend("public", "public"), []*basev0.Endpoint{
		{Module: "saas", Service: "frontend", Name: "http", Api: "grpc"},
	})
	require.ErrorContains(t, err, `reports API "grpc", but the producer manifest declares "http"`)
	require.ErrorContains(t, err, "correct the manifest's api")
}
