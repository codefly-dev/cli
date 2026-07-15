package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"

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
	// NamingScope is appended to the workspace name when deriving the
	// gRPC/REST port pair. Must match the scope the test SDK uses so the
	// client and server land on the same port. Empty string = no scope.
	NamingScope string
}

// NewServer builds the CLI's gRPC and REST servers. Ports are derived
// deterministically from the workspace name (plus optional naming scope)
// via network.CLIServerPort so different workspaces — and different test
// scopes within the same workspace — can run concurrently without
// colliding on port 10000.
func NewServer(input ServerData) (*CodeflyServer, error) {
	wsName := ""
	if input.Workspace != nil {
		wsName = input.Workspace.Name
	}
	if input.NamingScope != "" {
		wsName = wsName + "-" + input.NamingScope
	}
	grpcPort := network.CLIServerPort(wsName)
	restPort := network.CLIRestPort(wsName)
	config := go_grpc.Configuration{
		EndpointGrpc: loopbackEndpoint(grpcPort),
		EndpointRest: loopbackEndpoint(restPort),
	}
	server, err := go_grpc.NewServer(&config, input.Workspace)
	if err != nil {
		return nil, err
	}
	rest, err := go_grpc.NewHttpServer(&config, server)
	if err != nil {
		return nil, err
	}
	return &CodeflyServer{
		server: server,
		rest:   rest,
	}, nil
}

func loopbackEndpoint(port uint16) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
}

func (server *CodeflyServer) Start(ctx context.Context) error {
	golor.Println(`#(blue)[Starting server...]`)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		name string
		err  error
	}
	results := make(chan result, 2)
	go func() { results <- result{name: "REST", err: server.rest.Run(runCtx)} }()
	go func() { results <- result{name: "gRPC", err: server.server.Run(runCtx)} }()

	first := <-results
	cancel()
	second := <-results
	var failures []error
	for _, res := range []result{first, second} {
		if res.err != nil && !errors.Is(res.err, context.Canceled) {
			failures = append(failures, fmt.Errorf("%s server: %w", res.name, res.err))
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	if ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("%s server stopped unexpectedly", first.name)
}
