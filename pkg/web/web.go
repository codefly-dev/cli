package web

import (
	"context"

	"github.com/codefly-dev/cli/pkg/management"
	go_grpc "github.com/codefly-dev/cli/pkg/web/go-grpc"
	"github.com/codefly-dev/core/overview"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
)

type CodeflyServer struct {
	// app    *application.Application
	server *go_grpc.Server
	rest   *go_grpc.HttpServer
}

type ServerData struct {
	*management.Workspace
	*overview.DependencyGraph
}

func NewServer(input ServerData) (*CodeflyServer, error) {
	config := go_grpc.Configuration{
		EndpointGrpc:    ":10000",
		EndpointRest:    ":10001",
		Workspace:       input.Workspace,
		DependencyGraph: input.DependencyGraph,
	}
	server, err := go_grpc.NewServer(&config)
	if err != nil {
		return nil, err
	}
	rest, err := go_grpc.NewHttpServer(&config)
	if err != nil {
		return nil, err
	}
	return &CodeflyServer{
		server: server,
		rest:   rest,
	}, nil
}

func (server *CodeflyServer) Start(ctx context.Context) error {
	logger := shared.NewLogger("CodeflyServer.Start")
	golor.Println(`#(blue)[Starting server...]`)
	go func() {
		err := server.rest.Run(ctx)
		if err != nil {
			logger.Oops("got: %v", err)
		}
	}()
	return server.server.Run(ctx)
}
