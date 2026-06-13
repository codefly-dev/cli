package go_grpc

import (
	"context"

	"connectrpc.com/connect"
	cli "github.com/codefly-dev/core/generated/go/codefly/cli/v0"
	cliconnect "github.com/codefly-dev/core/generated/go/codefly/cli/v0/v0connect"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	observabilityv0 "github.com/codefly-dev/core/generated/go/codefly/observability/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// cliConnect adapts the gRPC *Server implementation to the Connect CLIHandler
// interface so the browser dashboard can talk Connect to the same impl that
// backs the gRPC server + REST gateway. Each method is a one-line delegation;
// Logs bridges the server-stream.
type cliConnect struct {
	s *Server
}

var _ cliconnect.CLIHandler = (*cliConnect)(nil)

// resp wraps a (msg, err) gRPC result into a Connect response.
func resp[T any](v *T, err error) (*connect.Response[T], error) {
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(v), nil
}

func (a *cliConnect) Ping(ctx context.Context, r *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
	return resp(a.s.Ping(ctx, r.Msg))
}
func (a *cliConnect) GetAgentInformation(ctx context.Context, r *connect.Request[cli.GetAgentInformationRequest]) (*connect.Response[agentv0.AgentInformation], error) {
	return resp(a.s.GetAgentInformation(ctx, r.Msg))
}
func (a *cliConnect) GetWorkspaceInventory(ctx context.Context, r *connect.Request[emptypb.Empty]) (*connect.Response[basev0.Workspace], error) {
	return resp(a.s.GetWorkspaceInventory(ctx, r.Msg))
}
func (a *cliConnect) GetWorkspaceServiceDependencyGraph(ctx context.Context, r *connect.Request[emptypb.Empty]) (*connect.Response[observabilityv0.GraphResponse], error) {
	return resp(a.s.GetWorkspaceServiceDependencyGraph(ctx, r.Msg))
}
func (a *cliConnect) GetWorkspacePublicModulesDependencyGraph(ctx context.Context, r *connect.Request[emptypb.Empty]) (*connect.Response[cli.MultiGraphResponse], error) {
	return resp(a.s.GetWorkspacePublicModulesDependencyGraph(ctx, r.Msg))
}
func (a *cliConnect) GetActive(ctx context.Context, r *connect.Request[emptypb.Empty]) (*connect.Response[cli.ActiveResponse], error) {
	return resp(a.s.GetActive(ctx, r.Msg))
}
func (a *cliConnect) GetAddresses(ctx context.Context, r *connect.Request[cli.GetAddressRequest]) (*connect.Response[cli.GetAddressResponse], error) {
	return resp(a.s.GetAddresses(ctx, r.Msg))
}
func (a *cliConnect) GetConfiguration(ctx context.Context, r *connect.Request[cli.GetConfigurationRequest]) (*connect.Response[cli.GetConfigurationResponse], error) {
	return resp(a.s.GetConfiguration(ctx, r.Msg))
}
func (a *cliConnect) GetDependenciesConfigurations(ctx context.Context, r *connect.Request[cli.GetConfigurationRequest]) (*connect.Response[cli.GetConfigurationsResponse], error) {
	return resp(a.s.GetDependenciesConfigurations(ctx, r.Msg))
}
func (a *cliConnect) GetRuntimeConfigurations(ctx context.Context, r *connect.Request[cli.GetConfigurationRequest]) (*connect.Response[cli.GetConfigurationsResponse], error) {
	return resp(a.s.GetRuntimeConfigurations(ctx, r.Msg))
}
func (a *cliConnect) GetDependenciesNetworkMappings(ctx context.Context, r *connect.Request[cli.GetNetworkMappingsRequest]) (*connect.Response[cli.GetNetworkMappingsResponse], error) {
	return resp(a.s.GetDependenciesNetworkMappings(ctx, r.Msg))
}
func (a *cliConnect) ActiveLogHistory(ctx context.Context, r *connect.Request[observabilityv0.LogRequest]) (*connect.Response[observabilityv0.LogResponse], error) {
	return resp(a.s.ActiveLogHistory(ctx, r.Msg))
}
func (a *cliConnect) GetFlowStatus(ctx context.Context, r *connect.Request[emptypb.Empty]) (*connect.Response[cli.FlowStatus], error) {
	return resp(a.s.GetFlowStatus(ctx, r.Msg))
}
func (a *cliConnect) StopFlow(ctx context.Context, r *connect.Request[cli.StopFlowRequest]) (*connect.Response[cli.StopFlowResponse], error) {
	return resp(a.s.StopFlow(ctx, r.Msg))
}
func (a *cliConnect) DestroyFlow(ctx context.Context, r *connect.Request[cli.DestroyFlowRequest]) (*connect.Response[cli.DestroyFlowResponse], error) {
	return resp(a.s.DestroyFlow(ctx, r.Msg))
}

// Logs bridges the gRPC server-stream onto the Connect server-stream.
func (a *cliConnect) Logs(ctx context.Context, _ *connect.Request[emptypb.Empty], stream *connect.ServerStream[observabilityv0.Log]) error {
	return a.s.Logs(&emptypb.Empty{}, &logsStreamShim{ctx: ctx, out: stream})
}

// logsStreamShim makes a Connect server-stream satisfy cli.CLI_LogsServer.
// Only Send + Context are exercised by Server.Logs; the rest are inherited from
// the (nil) embedded grpc.ServerStream and must not be called.
type logsStreamShim struct {
	grpc.ServerStream
	ctx context.Context
	out *connect.ServerStream[observabilityv0.Log]
}

func (s *logsStreamShim) Send(log *observabilityv0.Log) error { return s.out.Send(log) }
func (s *logsStreamShim) Context() context.Context            { return s.ctx }
