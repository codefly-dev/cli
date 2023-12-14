package go_grpc

import (
	"context"
	"fmt"
	"net"

	"google.golang.org/grpc/reflection"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"google.golang.org/protobuf/types/known/emptypb"

	cliobsv1 "github.com/codefly-dev/cli/proto/v1/go/observability"
	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/observability"
	agentsv1 "github.com/codefly-dev/core/proto/v1/go/agents"
	"github.com/codefly-dev/core/proto/v1/go/base"

	"github.com/codefly-dev/cli/pkg/application"
	"github.com/codefly-dev/golor"
	"google.golang.org/grpc"
)

type Configuration struct {
	EndpointGrpc       string
	EndpointRest       string
	RunningApplication *application.Application
}

type Server struct {
	cliobsv1.UnimplementedWebServer
	config     *Configuration
	gRPC       *grpc.Server
	logChannel chan *agentsv1.Log
	workspace  *configurations.Workspace
}

func (s *Server) GetProjectInventory(ctx context.Context, request *cliobsv1.ProjectInformationRequest) (*base.Project, error) {
	project, err := s.workspace.LoadProjectFromName(ctx, request.Project)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	view, err := observability.LoadProject(ctx, project)
	if err != nil {
		return nil, err
	}
	return view, nil
}

func (s *Server) GetServiceDependencyGraph(ctx context.Context, request *cliobsv1.ServiceDependencyGraphRequest) (*cliobsv1.GraphResponse, error) {
	project, err := s.workspace.LoadProjectFromName(ctx, request.Project)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	g, err := observability.NewDependencyGraph(ctx, project)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	resp := &cliobsv1.GraphResponse{}
	for _, node := range g.ServiceDependencyGraph.Nodes() {
		resp.Nodes = append(resp.Nodes, &cliobsv1.GraphNode{
			Id: node,
		})
	}
	for _, edge := range g.ServiceDependencyGraph.Edges() {
		resp.Edges = append(resp.Edges, &cliobsv1.GraphEdge{
			From: edge.From,
			To:   edge.To,
		})
	}
	return resp, nil
}

func (s *Server) LogHistory(ctx context.Context, request *cliobsv1.LogRequest) (*cliobsv1.LogResponse, error) {
	return nil, nil
}

func (s *Server) sendLogToClients(logEntry *agentsv1.Log) {
	s.logChannel <- logEntry
}

func (s *Server) Logs(empty *emptypb.Empty, server cliobsv1.Web_LogsServer) error {
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
		logChannel: make(chan *agentsv1.Log, bufferSize),
	}
	cliobsv1.RegisterWebServer(grpcServer, &s)
	reflection.Register(grpcServer)
	agents.RegisterLogCallback(s.sendLogToClients)
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
