package go_grpc

import (
	"context"
	"embed"
	"io/fs"
	"net"
	"net/http"

	cli "github.com/codefly-dev/core/generated/go/codefly/cli/v0"
	cliconnect "github.com/codefly-dev/core/generated/go/codefly/cli/v0/v0connect"
	"github.com/codefly-dev/golor"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/rs/cors"
	"github.com/tmc/grpc-websocket-proxy/wsproxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type HttpServer struct {
	config *Configuration
	impl   *Server
}

func NewHttpServer(c *Configuration, impl *Server) (*HttpServer, error) {
	server := &HttpServer{config: c, impl: impl}
	// Begin HTTP server (and proxy calls to gRPC server endpoint)
	return server, nil
}

func (s *HttpServer) Run(ctx context.Context) error {
	golor.Template(s.config).Println(`#(blue,bold)[🚀 Starting codefly REST server at]: #(italic,white)[{{ .EndpointRest }}]`)

	gwMux := runtime.NewServeMux()

	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	err := cli.RegisterCLIHandlerFromEndpoint(ctx, gwMux, s.config.EndpointGrpc, opts)
	if err != nil {
		return err
	}
	// With websocket
	gwSocket := wsproxy.WebsocketProxy(gwMux)
	// Register gRPC-Gateway on the main mux
	// Set up CORS
	gwHandler := cors.Default().Handler(gwSocket)
	// Serve static assets for Work.js app
	// Add a new http.ServeMux

	mux := http.NewServeMux()
	mux.Handle("/api/", http.StripPrefix("/api", gwHandler))

	// Connect endpoint for the browser dashboard (Connect-ES). Serves the same
	// CLI service impl over the Connect/gRPC-Web protocols at /codefly.cli.v0.CLI/.
	// More specific than "/" so it wins over the static file server below.
	if s.impl != nil {
		path, connectHandler := cliconnect.NewCLIHandler(&cliConnect{s: s.impl})
		mux.Handle(path, connectHandler)
	}

	// Route requests to the file server.
	outFs, err := fs.Sub(content, "out")
	if err != nil {
		return err
	}
	fileServer := http.FileServer(http.FS(outFs))

	// Route requests to the file server.
	mux.Handle("/", http.StripPrefix("/", fileServer))

	golor.Println(`Serving #(bold,blue)[codefly] webserver at http://localhost:10001`)
	// Begin HTTP server (and proxy calls to gRPC server endpoint)

	handler := cors.Default().Handler(mux)

	srv := &http.Server{Addr: s.config.EndpointRest, Handler: handler}
	lis, err := net.Listen("tcp", s.config.EndpointRest)
	if err != nil {
		return err
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(lis)
	}()

	select {
	case <-ctx.Done():
		if err := srv.Shutdown(context.Background()); err != nil {
			return err
		}
		return nil
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}
}

//go:embed out/*
var content embed.FS
