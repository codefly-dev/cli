package go_grpc

import (
	"context"
	"fmt"
	"net"

	managementv1 "github.com/codefly-dev/cli/proto/v1/go/management"
	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/overview"
	agentsv1 "github.com/codefly-dev/core/proto/v1/go/agents"

	"github.com/codefly-dev/cli/pkg/application"
	"github.com/codefly-dev/cli/pkg/management"
	"github.com/codefly-dev/golor"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

type Configuration struct {
	EndpointGrpc       string
	EndpointRest       string
	RunningApplication *application.Application

	Workspace *management.Workspace

	DependencyGraph *overview.DependencyGraph
}

type Server struct {
	managementv1.UnsafeWebServer
	config     *Configuration
	gRPC       *grpc.Server
	logChannel chan *agentsv1.Log
}

func (s *Server) GetDependencyGraph(ctx context.Context, request *managementv1.DependencyGraphRequest) (*managementv1.GraphResponse, error) {
	g := &managementv1.GraphResponse{}
	for _, node := range s.config.DependencyGraph.Nodes() {
		g.Nodes = append(g.Nodes, &managementv1.GraphNode{
			Id: node,
		})
	}
	for _, edge := range s.config.DependencyGraph.Edges() {
		g.Edges = append(g.Edges, &managementv1.GraphEdge{
			From: edge.From,
			To:   edge.To,
		})
	}
	return g, nil
}

func (s *Server) LogHistory(ctx context.Context, request *managementv1.LogRequest) (*managementv1.LogResponse, error) {
	return nil, nil
}

func (s *Server) sendLogToClients(logEntry *agentsv1.Log) {
	s.logChannel <- logEntry
}

func (s *Server) Logs(empty *emptypb.Empty, server managementv1.Web_LogsServer) error {
	for logEntry := range s.logChannel {
		if err := server.Send(logEntry); err != nil {
			return err
		}
		// handle context cancellation or timeout if necessary
	}
	return nil
}

func (s *Server) GetProjectInformation(ctx context.Context, request *managementv1.ProjectInformationRequest) (*managementv1.ProjectInformationResponse, error) {
	project, err := s.config.Workspace.GetProject()
	if err != nil {
		return nil, fmt.Errorf("cannot get project information: %s", err)
	}
	return &managementv1.ProjectInformationResponse{Project: project}, nil
}

func (s *Server) GetApplicationInformation(ctx context.Context, request *managementv1.ApplicationInformationRequest) (*managementv1.ApplicationInformationResponse, error) {
	app, err := s.config.Workspace.GetApplication(request.Application)
	if err != nil {
		return nil, fmt.Errorf("cannot get app information: %s", err)
	}
	return &managementv1.ApplicationInformationResponse{Application: app}, nil
}

func (s *Server) GetAgentUsage(ctx context.Context, request *managementv1.AgentUsageRequest) (*managementv1.AgentUsageResponse, error) {
	base := fmt.Sprintf("%s/%s", request.Publisher, request.Name)
	usage, err := s.config.Workspace.Usage(base)
	if err != nil {
		return nil, fmt.Errorf("cannot get agent usage: %s", err)
	}
	return &managementv1.AgentUsageResponse{Usage: usage}, nil
}

func (s *Server) GetServiceInformation(ctx context.Context, request *managementv1.ServiceInformationRequest) (*managementv1.ServiceInformationResponse, error) {
	return &managementv1.ServiceInformationResponse{}, nil
}

func NewServer(c *Configuration) (*Server, error) {
	grpcServer := grpc.NewServer()
	bufferSize := 100
	s := Server{
		config:     c,
		gRPC:       grpcServer,
		logChannel: make(chan *agentsv1.Log, bufferSize),
	}
	managementv1.RegisterWebServer(grpcServer, &s)
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
