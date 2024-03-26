package go_grpc

import (
	"context"
	"fmt"
	"net"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"google.golang.org/grpc/reflection"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/cli/pkg/services/manager"
	"github.com/codefly-dev/cli/pkg/services/services"
	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/configurations"
	cli "github.com/codefly-dev/core/generated/go/cli/v0"
	"github.com/codefly-dev/core/wool"

	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	observabilityv0 "github.com/codefly-dev/core/generated/go/observability/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"

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
	workspace  *configurations.Workspace
	Wool       *wool.Wool
}

func (s *Server) Ping(ctx context.Context, empty *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (s *Server) StopFlow(ctx context.Context, empty *emptypb.Empty) (*emptypb.Empty, error) {
	err := manager.CurrentFlow().Stop()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) GetFlowStatus(ctx context.Context, empty *emptypb.Empty) (*cli.FlowStatus, error) {
	ready := manager.CurrentFlow().Ready(ctx)
	return &cli.FlowStatus{
		Ready: ready,
	}, nil
}

func (s *Server) GetSharedConfiguration(ctx context.Context, req *cli.GetConfigurationRequest) (*cli.GetConfigurationResponse, error) {
	flow := manager.CurrentFlow()
	if flow == nil {
		return nil, status.Error(codes.Internal, "nothing running")
	}
	unique := configurations.ServiceUnique(req.Application, req.Service)
	svc := flow.ServiceFromUnique(unique)
	if svc == nil {
		return nil, status.Error(codes.Internal, "service not found")
	}
	confs, err := flow.ConfigurationManager.GetSharedServiceConfiguration(ctx, svc)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cli.GetConfigurationResponse{
		Configurations: confs,
	}, nil
}

func (s *Server) GetAddresses(ctx context.Context, req *cli.GetAddressRequest) (*cli.GetAddressResponse, error) {
	flow := manager.CurrentFlow()
	if flow == nil {
		return nil, status.Error(codes.Internal, "nothing running")
	}
	address, err := flow.GetAddressForEndpoint(ctx, req.Application, req.Service, req.Endpoint)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cli.GetAddressResponse{
		Address: address,
	}, nil
}

/* Active information */

func (s *Server) GetActive(ctx context.Context, empty *emptypb.Empty) (*cli.ActiveResponse, error) {
	active, err := common.LoadActiveContext(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cli.ActiveResponse{
		Project:     active.Project.Name,
		Application: active.Application.Name,
		Service:     active.Service.Name,
	}, nil

}

func (s *Server) ActiveLogHistory(ctx context.Context, request *observabilityv0.LogRequest) (*observabilityv0.LogResponse, error) {
	return nil, status.Error(codes.Internal, "TBI")
}

/* Overall information */

func (s *Server) GetAgentInformation(ctx context.Context, request *cli.GetAgentInformationRequest) (*agentv0.AgentInformation, error) {
	agent, err := configurations.ParseAgent(ctx, configurations.ServiceAgent, request.Agent)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	loaded, err := services.LoadAgent(ctx, agent)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return loaded.GetAgentInformation(ctx, &agentv0.AgentInformationRequest{})

}

func (s *Server) GetProjects(ctx context.Context, empty *emptypb.Empty) (*cli.GetProjectsResponse, error) {
	var projects []string
	if s.workspace == nil {
		project := common.Project(ctx)
		if project == nil {
			return nil, status.Error(codes.Internal, "no project")
		}
		projects = []string{project.Name}
	} else {
		for _, project := range s.workspace.Projects {
			projects = append(projects, project.Name)
		}
	}
	return &cli.GetProjectsResponse{
		Projects: projects,
	}, nil
}

func (s *Server) getProject(ctx context.Context, name string) (*configurations.Project, error) {
	if s.workspace == nil {
		project := common.Project(ctx)
		if project == nil {
			return nil, status.Error(codes.Internal, "no project")
		}
		if project.Name != name {
			return nil, status.Error(codes.Internal, "project mismatch")
		}
		return project, nil
	}
	return s.workspace.LoadProjectFromName(ctx, name)
}

func (s *Server) GetProjectInventory(ctx context.Context, request *cli.ProjectRequest) (*basev0.Project, error) {
	project, err := s.getProject(ctx, request.Project)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	view, err := architecture.LoadProject(ctx, project)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return view, nil
}

func (s *Server) GetProjectServiceDependencyGraph(ctx context.Context, request *cli.ProjectRequest) (*observabilityv0.GraphResponse, error) {
	return &observabilityv0.GraphResponse{}, nil
}

func (s *Server) GetProjectPublicApplicationsDependencyGraph(ctx context.Context, request *cli.ProjectRequest) (*cli.MultiGraphResponse, error) {
	project, err := s.getProject(ctx, request.Project)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	gs, err := architecture.LoadPublicApplicationGraph(ctx, project)
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

func (s *Server) ProcessWithSource(source *wool.Identifier, log *wool.Log) {
	if source.IsSystem() {
		return
	}
	service, err := configurations.ParseServiceUnique(source.Unique)
	if err != nil {
		s.Wool.Error("cannot parse service from source", wool.Field("source", source), wool.Field("error", err))
		return
	}
	logEntry := &observabilityv0.Log{
		At:          timestamppb.New(time.Now()),
		Application: service.Application,
		Service:     service.Name,
		Message:     log.String(),
		Kind:        observabilityv0.Log_SERVICE,
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

func NewServer(c *Configuration, w *configurations.Workspace) (*Server, error) {
	grpcServer := grpc.NewServer()
	bufferSize := 100
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
	w := wool.Get(ctx).In("webServer")
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
