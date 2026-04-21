package web

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/cli"
	go_grpc "github.com/codefly-dev/cli/pkg/web/go-grpc"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/golor"
)

type CodeflyServer struct {
	// app    *module.Module
	server *go_grpc.Server
	rest   *go_grpc.HttpServer
}

type ServerData struct {
	Workspace *resources.Workspace
}

// NewServer builds the CLI's gRPC and REST servers. Ports are derived
// deterministically from the workspace name via network.CLIServerPort
// so different workspaces can run concurrently without colliding on
// port 10000. Override with CODEFLY_CLI_SERVER_PORT when an explicit
// port is required.
func NewServer(input ServerData) (*CodeflyServer, error) {
	wsName := ""
	if input.Workspace != nil {
		wsName = input.Workspace.Name
	}
	grpcPort := network.CLIServerPort(wsName)
	restPort := network.CLIRestPort(wsName)
	config := go_grpc.Configuration{
		EndpointGrpc: fmt.Sprintf(":%d", grpcPort),
		EndpointRest: fmt.Sprintf(":%d", restPort),
	}
	server, err := go_grpc.NewServer(&config, input.Workspace)
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
	golor.Println(`#(blue)[Starting server...]`)
	go func() {
		err := server.rest.Run(ctx)
		if err != nil {
			cli.ExitOnError(err, "cannot start rest server")
		}
	}()
	return server.server.Run(ctx)
}
