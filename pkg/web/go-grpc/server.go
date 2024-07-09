package go_grpc

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"google.golang.org/grpc/reflection"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/services/manager"
	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/architecture"
	cli "github.com/codefly-dev/core/generated/go/codefly/cli/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	observabilityv0 "github.com/codefly-dev/core/generated/go/codefly/observability/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"

	"google.golang.org/grpc"
)

type Configuration struct {
	EndpointGrpc string
	EndpointRest string
}

type Server struct {
	cli.UnsafeCLIServer
	config     *Configuration
	gRPC       *grpc.Server
	logChannel chan *observabilityv0.Log
	workspace  *resources.Workspace
	Wool       *wool.Wool
}

func (s *Server) Ping(ctx context.Context, empty *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (s *Server) StopFlow(ctx context.Context, req *cli.StopFlowRequest) (*cli.StopFlowResponse, error) {
	err := manager.CurrentFlow().Stop()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cli.StopFlowResponse{}, nil
}

func (s *Server) DestroyFlow(ctx context.Context, req *cli.DestroyFlowRequest) (*cli.DestroyFlowResponse, error) {
	err := manager.CurrentFlow().Stop()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cli.DestroyFlowResponse{}, nil
}

func (s *Server) GetFlowStatus(ctx context.Context, empty *emptypb.Empty) (*cli.FlowStatus, error) {
	ready := manager.CurrentFlow().Ready(ctx)
	return &cli.FlowStatus{
		Ready: ready,
	}, nil
}

func (s *Server) GetDependenciesNetworkMappings(ctx context.Context, req *cli.GetNetworkMappingsRequest) (*cli.GetNetworkMappingsResponse, error) {
	flow := manager.CurrentFlow()
	if flow == nil {
		return nil, status.Error(codes.Internal, "nothing running")
	}
	unique := resources.ServiceUnique(req.Module, req.Service)
	svc, err := flow.ServiceFromUnique(unique)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	mappings, err := flow.GetDependenciesNetworkMappingsFor(ctx, svc)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cli.GetNetworkMappingsResponse{NetworkMappings: mappings}, nil
}

func (s *Server) GetConfiguration(ctx context.Context, req *cli.GetConfigurationRequest) (*cli.GetConfigurationResponse, error) {
	flow := manager.CurrentFlow()
	if flow == nil {
		return nil, status.Error(codes.Internal, "nothing running")
	}
	unique := resources.ServiceUnique(req.Module, req.Service)
	svc, err := flow.ServiceFromUnique(unique)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	conf, err := flow.ConfigurationManager.GetServiceConfiguration(ctx, svc)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cli.GetConfigurationResponse{
		Configuration: conf,
	}, nil
}

func (s *Server) GetDependenciesConfigurations(ctx context.Context, req *cli.GetConfigurationRequest) (*cli.GetConfigurationsResponse, error) {
	flow := manager.CurrentFlow()
	if flow == nil {
		return nil, status.Error(codes.Internal, "nothing running")
	}
	unique := resources.ServiceUnique(req.Module, req.Service)
	svc, err := flow.ServiceFromUnique(unique)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	confs, err := flow.SharedState.GetDependentConfigurationsFor(ctx, svc)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cli.GetConfigurationsResponse{
		Configurations: confs,
	}, nil
}

func (s *Server) GetRuntimeConfigurations(ctx context.Context, req *cli.GetConfigurationRequest) (*cli.GetConfigurationsResponse, error) {
	flow := manager.CurrentFlow()
	if flow == nil {
		return nil, status.Error(codes.Internal, "nothing running")
	}
	unique := resources.ServiceUnique(req.Module, req.Service)
	confs, err := flow.ConfigurationManager.GetSharedServiceConfiguration(ctx, unique)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cli.GetConfigurationsResponse{
		Configurations: confs,
	}, nil
}

func (s *Server) GetAddresses(ctx context.Context, req *cli.GetAddressRequest) (*cli.GetAddressResponse, error) {
	flow := manager.CurrentFlow()
	if flow == nil {
		return nil, status.Error(codes.Internal, "nothing running")
	}
	address, err := flow.GetAddressForEndpoint(ctx, req.Module, req.Service, req.Endpoint)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cli.GetAddressResponse{
		Address: address,
	}, nil
}

/* Active information */

func (s *Server) GetActive(ctx context.Context, empty *emptypb.Empty) (*cli.ActiveResponse, error) {
	flow := manager.CurrentFlow()
	if flow == nil {
		return nil, status.Error(codes.Internal, "nothing running")
	}
	return &cli.ActiveResponse{
		Workspace: flow.ActiveWorkspace().Name,
		Module:    flow.Origin().Module,
		Service:   flow.Origin().Name,
	}, nil

}

func (s *Server) ActiveLogHistory(ctx context.Context, request *observabilityv0.LogRequest) (*observabilityv0.LogResponse, error) {
	return nil, status.Error(codes.Internal, "TBI")
}

/* Overall information */

func (s *Server) GetAgentInformation(ctx context.Context, request *cli.GetAgentInformationRequest) (*agentv0.AgentInformation, error) {
	agent, err := resources.ParseAgent(ctx, resources.ServiceAgent, request.Agent)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	loaded, err := services.LoadAgent(ctx, agent)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return loaded.GetAgentInformation(ctx, &agentv0.AgentInformationRequest{})

}

func (s *Server) GetWorkspaceInventory(ctx context.Context, request *emptypb.Empty) (*basev0.Workspace, error) {
	workspace := common.RequireWorkspace(ctx)
	view, err := architecture.LoadWorkspace(ctx, workspace)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return view, nil
}

func (s *Server) GetWorkspaceServiceDependencyGraph(ctx context.Context, request *emptypb.Empty) (*observabilityv0.GraphResponse, error) {
	return &observabilityv0.GraphResponse{}, nil
}

func (s *Server) GetWorkspacePublicModulesDependencyGraph(ctx context.Context, request *emptypb.Empty) (*cli.MultiGraphResponse, error) {
	workspace := common.RequireWorkspace(ctx)
	gs, err := architecture.LoadPublicModuleGraph(ctx, workspace)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	resp := &cli.MultiGraphResponse{}
	for _, g := range gs {
		resp.Graphs = append(resp.Graphs, architecture.ToGraphResponse(g))
	}
	return resp, nil
}

func (s *Server) LogHistory(ctx context.Context, request *observabilityv0.LogRequest) (*observabilityv0.LogResponse, error) {
	return nil, nil
}

// removeANSICodes strips ANSI escape codes from the input string.
func removeANSICodes(input string) string {
	// ANSI escape codes start with the escape character (ASCII 27, octal \033)
	// followed by '[' and end with 'm'. This regex also accounts for other sequences.
	re := regexp.MustCompile(`\x1B\[[0-9;]*[a-zA-Z]`)
	return re.ReplaceAllString(input, "")
}

func (s *Server) ProcessWithSource(source *wool.Identifier, log *wool.Log) {
	if source.IsSystem() {
		return
	}
	service, err := resources.ParseServiceWithOptionalModule(source.Unique)
	if err != nil {
		s.Wool.Error("cannot parse service from source", wool.Field("source", source), wool.Field("error", err))
		return
	}
	if source.Kind != "service" {
		return
	}
	logEntry := &observabilityv0.Log{
		At:      timestamppb.New(time.Now()),
		Module:  service.Module,
		Service: service.Name,
		Message: removeANSICodes(log.String()),
		Kind:    source.Kind,
	}
	go func() {
		s.logChannel <- logEntry
	}()
}

func (s *Server) Logs(empty *emptypb.Empty, server cli.CLI_LogsServer) error {
	for logEntry := range s.logChannel {
		if err := server.Send(logEntry); err != nil {
			return err
		}
	}
	return nil
}

func NewServer(c *Configuration, w *resources.Workspace) (*Server, error) {
	grpcServer := grpc.NewServer()
	bufferSize := 10000
	s := Server{
		config:     c,
		workspace:  w,
		gRPC:       grpcServer,
		logChannel: make(chan *observabilityv0.Log, bufferSize),
	}
	cli.RegisterCLIServer(grpcServer, &s)
	reflection.Register(grpcServer)
	return &s, nil
}

func (s *Server) Run(ctx context.Context) error {
	w := wool.Get(ctx).In("cli.Server")
	s.Wool = w
	agents.AddProcessor(s)
	lis, err := net.Listen("tcp", s.config.EndpointGrpc)
	if err != nil {
		return fmt.Errorf("failed to listen: %v", err)
	}

	if err := s.gRPC.Serve(lis); err != nil {
		return fmt.Errorf("failed to serve: %s", err)
	}
	return nil
}
