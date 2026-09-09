package go_grpc

import (
	"context"
	"embed"
	"io/fs"
	"net"
	"net/http"
	"sync"

	connectcors "connectrpc.com/cors"
	cliconnect "github.com/codefly-dev/core/generated/go/codefly/cli/v0/v0connect"
	"github.com/codefly-dev/golor"
	"github.com/rs/cors"
)

type HttpServer struct {
	config *Configuration
	impl   *Server

	// listenerMu guards listener; see Server.listenerMu.
	listenerMu sync.Mutex
	listener   net.Listener
}

func NewHttpServer(c *Configuration, impl *Server) (*HttpServer, error) {
	server := &HttpServer{config: c, impl: impl}
	// Begin HTTP server (and proxy calls to gRPC server endpoint)
	return server, nil
}

// Address is the REST endpoint this server binds, e.g. "127.0.0.1:10001".
func (s *HttpServer) Address() string {
	return s.config.EndpointRest
}

// Listen claims the REST address, returning the listener it holds. See
// Server.Listen for why binding is a separate, callable step.
func (s *HttpServer) Listen() (net.Listener, error) {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	if s.listener != nil {
		return s.listener, nil
	}
	lis, err := net.Listen("tcp", s.config.EndpointRest)
	if err != nil {
		return nil, err
	}
	s.listener = lis
	return lis, nil
}

// Close releases the claimed address. Safe to call at any point, including
// while Run is serving on it.
func (s *HttpServer) Close() {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	if s.listener == nil {
		return
	}
	_ = s.listener.Close()
	s.listener = nil
}

func (s *HttpServer) Run(ctx context.Context) error {
	golor.Template(s.config).Println(`#(blue,bold)[Dashboard:] #(italic,white)[http://{{ .EndpointRest }}]`)

	handler, err := s.handler()
	if err != nil {
		return err
	}

	srv := &http.Server{Addr: s.config.EndpointRest, Handler: handler}
	lis, err := s.Listen()
	if err != nil {
		return err
	}
	defer s.Close()

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

// handler builds the dashboard HTTP handler: the Connect CLI service mounted at
// its service path, the embedded static dashboard served at "/", and the whole
// thing wrapped in Connect-aware CORS.
func (s *HttpServer) handler() (http.Handler, error) {
	mux := http.NewServeMux()

	// Connect endpoint for the browser dashboard (Connect-ES) — the CLI service
	// over the Connect / gRPC-Web protocols at /codefly.cli.v0.CLI/. This is the
	// ONLY API the dashboard uses; it fully replaces the old grpc-gateway REST
	// endpoint (/api/), which has been removed.
	if s.impl != nil {
		path, connectHandler := cliconnect.NewCLIHandler(&cliConnect{s: s.impl})
		mux.Handle(path, connectHandler)
	}

	// Static dashboard assets (embedded Vite build). "/" is the least specific
	// pattern, so the Connect handler above wins for RPC calls.
	outFs, err := fs.Sub(content, "out")
	if err != nil {
		return nil, err
	}
	mux.Handle("/", http.StripPrefix("/", http.FileServer(http.FS(outFs))))

	// Connect-aware CORS so the dashboard works cross-origin in dev (`vite dev`
	// hitting the CLI on its web port). The CLI binds localhost only, so allowing
	// any origin is safe — and required for the browser to send Connect's custom
	// headers (Connect-Protocol-Version, …) and read the streamed response
	// headers. Production is same-origin.
	return cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: connectcors.AllowedMethods(),
		AllowedHeaders: connectcors.AllowedHeaders(),
		ExposedHeaders: connectcors.ExposedHeaders(),
	}).Handler(mux), nil
}

//go:embed out/*
var content embed.FS
