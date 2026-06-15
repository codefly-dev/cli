package go_grpc

import (
	"context"
	"embed"
	"io/fs"
	"net"
	"net/http"

	connectcors "connectrpc.com/cors"
	cliconnect "github.com/codefly-dev/core/generated/go/codefly/cli/v0/v0connect"
	"github.com/codefly-dev/golor"
	"github.com/rs/cors"
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

	mux := http.NewServeMux()

	// Connect endpoint for the browser dashboard (Connect-ES) — the CLI service
	// over the Connect / gRPC-Web protocols at /codefly.cli.v0.CLI/. This is the
	// ONLY API the dashboard uses; it fully replaces the old grpc-gateway REST
	// endpoint (/api/), which has been removed.
	if s.impl != nil {
		path, connectHandler := cliconnect.NewCLIHandler(&cliConnect{s: s.impl})
		mux.Handle(path, connectHandler)
	}

	// Static dashboard assets (embedded Next.js export). "/" is the least
	// specific pattern, so the Connect handler above wins for RPC calls.
	outFs, err := fs.Sub(content, "out")
	if err != nil {
		return err
	}
	mux.Handle("/", http.StripPrefix("/", http.FileServer(http.FS(outFs))))

	golor.Println(`Serving #(bold,blue)[codefly] webserver at http://localhost:10001`)
	// Begin HTTP server (and proxy calls to gRPC server endpoint)

	// Connect-aware CORS so the dashboard works cross-origin in dev
	// (`next dev` on :3000 hitting the CLI on its web port). The CLI binds
	// localhost only, so allowing any origin is safe — and required for the
	// browser to send Connect's custom headers (Connect-Protocol-Version, …)
	// and read the streamed response headers. Production is same-origin.
	handler := cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: connectcors.AllowedMethods(),
		AllowedHeaders: connectcors.AllowedHeaders(),
		ExposedHeaders: connectcors.ExposedHeaders(),
	}).Handler(mux)

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
