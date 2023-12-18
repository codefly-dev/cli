package go_grpc

import (
	"context"
	"fmt"
	"net"

	"google.golang.org/grpc/reflection"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"google.golang.org/protobuf/types/known/emptypb"

	web "github.com/codefly-dev/cli/generated/go/web/v1"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/configurations"

	basev1 "github.com/codefly-dev/core/generated/go/base/v1"
	observabilityv1 "github.com/codefly-dev/core/generated/go/observability/v1"

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

func (s *Server) GetProjects(ctx context.Context, empty *emptypb.Empty) (*web.GetProjectsResponse, error) {
	var projects []string
	for _, project := range s.workspace.Projects {
		projects = append(projects, project.Name)
	}
	return &web.GetProjectsResponse{
		Projects: projects,
	}, nil
}

func (s *Server) GetProjectInventory(ctx context.Context, request *web.ProjectInventoryRequest) (*basev1.Project, error) {
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

func (s *Server) GetProjectServiceDependencyGraph(ctx context.Context, request *web.ServiceDependencyGraphRequest) (*observabilityv1.GraphResponse, error) {
	project, err := s.workspace.LoadProjectFromName(ctx, request.Project)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	g, err := architecture.NewDependencyGraph(ctx, project)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	resp := &observabilityv1.GraphResponse{}
	for _, node := range g.ServiceDependencyGraph.Nodes() {
		resp.Nodes = append(resp.Nodes, &observabilityv1.GraphNode{
			Id: node,
		})
	}
	for _, edge := range g.ServiceDependencyGraph.Edges() {
		resp.Edges = append(resp.Edges, &observabilityv1.GraphEdge{
			From: edge.From,
			To:   edge.To,
		})
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
