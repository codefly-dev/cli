package go_grpc

import (
	"context"
	"testing"
	"time"
)

func TestServerRunHonorsCancelledContext(t *testing.T) {
	server, err := NewServer(&Configuration{EndpointGrpc: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gRPC server ignored context cancellation")
	}
}
