package go_grpc

import (
	"context"
	"fmt"
	"net"

	"google.golang.org/grpc/reflection"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/codefly-dev/cli/cmd/common"
	web "github.com/codefly-dev/cli/generated/go/web/v1"
	"github.com/codefly-dev/cli/pkg/services/runner"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/configurations"

	basev1 "github.com/codefly-dev/core/generated/go/base/v1"
	observabilityv1 "github.com/codefly-dev/core/generated/go/observability/v1"
	agentv1 "github.com/codefly-dev/core/generated/go/services/agent/v1"

	"github.com/codefly-dev/golor"
	"google.golang.org/grpc"
)

type Configuration struct {
	EndpointGrpc string
	EndpointRest string
}

type Server struct {
	web.UnsafeWebServer
	config     *Configuration
	gRPC       *grpc.Server
	logChannel chan *observabilityv1.Log
	workspace  *configurations.Workspace
}

func (s *Server) GetRunningInformation(ctx context.Context, empty *emptypb.Empty) (*web.RunningInformationResponse, error) {
	infos, err := runner.AgentPIDs(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	var running []*web.RunningInformation
	for _, info := range infos {
		running = append(running, &web.RunningInformation{
			Application: info.Application,
			Service:     info.Service,
			AgentPid:    int32(info.AgentPID),
		})
	}
	return &web.RunningInformationResponse{
		Running: running,
	}, nil
}

/* Active information */

func (s *Server) GetActive(ctx context.Context, empty *emptypb.Empty) (*web.ActiveResponse, error) {
	active, err := common.LoadActiveContext(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &web.ActiveResponse{
		Project:     active.Project,
		Application: active.Application,
		Service:     active.Service,
	}, nil

}

func (s *Server) ActiveLogHistory(ctx context.Context, request *observabilityv1.LogRequest) (*observabilityv1.LogResponse, error) {
	return nil, status.Error(codes.Internal, "TBI")
}

/* Overall information */

func (s *Server) GetAgentInformation(ctx context.Context, request *web.GetAgentInformationRequest) (*agentv1.AgentInformation, error) {
	agent, err := configurations.ParseAgent(ctx, configurations.ServiceAgent, request.Agent)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	loaded, err := services.LoadAgent(ctx, agent)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return loaded.GetAgentInformation(ctx, &agentv1.AgentInformationRequest{})

}

func (s *Server) GetProjects(ctx context.Context, empty *emptypb.Empty) (*web.GetProjectsResponse, error) {
	var projects []string
	for _, project := range s.workspace.Projects {
		projects = append(projects, project.Name)
	}
	return &web.GetProjectsResponse{
		Projects: projects,
	}, nil
}

func (s *Server) GetProjectInventory(ctx context.Context, request *web.ProjectRequest) (*basev1.Project, error) {
	project, err := s.workspace.LoadProjectFromName(ctx, request.Project)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	view, err := architecture.LoadProject(ctx, project)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return view, nil
}

func (s *Server) GetProjectServiceDependencyGraph(ctx context.Context, request *web.ProjectRequest) (*observabilityv1.GraphResponse, error) {
	project, err := s.workspace.LoadProjectFromName(ctx, request.Project)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	g, err := architecture.LoadServiceGraph(ctx, project)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return architecture.ToGraphResponse(g), nil
}

func (s *Server) GetProjectPublicApplicationsDependencyGraph(ctx context.Context, request *web.ProjectRequest) (*web.MultiGraphResponse, error) {
	project, err := s.workspace.LoadProjectFromName(ctx, request.Project)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	gs, err := architecture.LoadPublicApplicationGraph(ctx, project)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	resp := &web.MultiGraphResponse{}
	for _, g := range gs {
		resp.Graphs = append(resp.Graphs, architecture.ToGraphResponse(g))
	}
	return resp, nil
}

func (s *Server) LogHistory(ctx context.Context, request *observabilityv1.LogRequest) (*observabilityv1.LogResponse, error) {
	return nil, nil
}

func (s *Server) sendLogToClients(logEntry *observabilityv1.Log) {
	s.logChannel <- logEntry
}

func (s *Server) Logs(empty *emptypb.Empty, server web.Web_LogsServer) error {
	for logEntry := range s.logChannel {
		if err := server.Send(logEntry); err != nil {
			return err
		}
		// handle context cancellation or timeout if necessary
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
		logChannel: make(chan *observabilityv1.Log, bufferSize),
	}
	web.RegisterWebServer(grpcServer, &s)
	reflection.Register(grpcServer)
	return &s, nil
}

func (s *Server) Run(ctx context.Context) error {
	golor.Println(`#(blue,bold)[🚀 Starting codefly gRPC server at]: #(italic,white)[{{ .EndpointGrpc }}]`,
		map[string]string{"EndpointGrpc": s.config.EndpointGrpc})
	lis, err := net.Listen("tcp", s.config.EndpointGrpc)
	if err != nil {
		return fmt.Errorf("failed to listen: %v", err)
	}

	if err := s.gRPC.Serve(lis); err != nil {
		return fmt.Errorf("failed to serve: %s", err)
	}
	return nil
}
