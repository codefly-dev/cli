package go_grpc

import (
	"context"
	"embed"
	"io/fs"
	"net/http"

	web "github.com/codefly-dev/cli/generated/go/web/v1"
	"github.com/codefly-dev/golor"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/rs/cors"
	"github.com/tmc/grpc-websocket-proxy/wsproxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type HttpServer struct {
	config *Configuration
}

func NewHttpServer(c *Configuration) (*HttpServer, error) {
	server := &HttpServer{config: c}
	// Start HTTP server (and proxy calls to gRPC server endpoint)
	return server, nil
}

func (s *HttpServer) Run(ctx context.Context) error {
	golor.Println(`#(blue,bold)[🚀 Starting codefly REST server at]: #(italic,white)[{{ .EndpointRest }}]`,
		map[string]string{"EndpointRest": s.config.EndpointRest})
	gwMux := runtime.NewServeMux()

	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	err := web.RegisterWebHandlerFromEndpoint(ctx, gwMux, s.config.EndpointGrpc, opts)
	if err != nil {
		return err
	}
	// With websocket
	gwSocket := wsproxy.WebsocketProxy(gwMux)
	// Register gRPC-Gateway on the main mux
	// Set up CORS
	gwHandler := cors.Default().Handler(gwSocket)
	// Serve static assets for Next.js app
	// Add a new http.ServeMux

	mux := http.NewServeMux()
	mux.Handle("/api/", http.StripPrefix("/api", gwHandler))

	// Route requests to the file server.
	outFs, err := fs.Sub(content, "out")
	if err != nil {
		return err
	}
	fileServer := http.FileServer(http.FS(outFs))

	// Route requests to the file server.
	mux.Handle("/", http.StripPrefix("/", fileServer))

	golor.Println(`Serving #(bold,blue)[codefly] webserver at http://localhost:10001`)
	// Start HTTP server (and proxy calls to gRPC server endpoint)

	handler := cors.Default().Handler(mux)
	err = http.ListenAndServe(s.config.EndpointRest, handler)
	if err != nil {
		return err
	}
	return nil
}

//go:embed out/*
var content embed.FS
