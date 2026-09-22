package orchestration

import (
	"context"
	"net"
	"testing"
	"time"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type teardownDeadlinePeer struct {
	runtimev0.UnimplementedRuntimeServer
	deadlines chan time.Duration
}

func (s *teardownDeadlinePeer) record(ctx context.Context) {
	deadline, _ := ctx.Deadline()
	s.deadlines <- time.Until(deadline)
}

func (s *teardownDeadlinePeer) Stop(ctx context.Context, _ *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	s.record(ctx)
	return &runtimev0.StopResponse{}, nil
}

func (s *teardownDeadlinePeer) Destroy(ctx context.Context, _ *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	s.record(ctx)
	return &runtimev0.DestroyResponse{}, nil
}

func TestRunnerTeardownDoesNotShortenThePhaseToTheBackendTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	peer := &teardownDeadlinePeer{deadlines: make(chan time.Duration, 1)}
	server := grpc.NewServer()
	runtimev0.RegisterRuntimeServer(server, peer)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close() })
	runner := runnerWithRuntimeClient(runtimev0.NewRuntimeClient(connection))
	for _, test := range []struct {
		name   string
		budget time.Duration
		invoke func(context.Context) (*OutputProperty, error)
	}{
		{"stop", defaultStopPhaseBudget, runner.Stop},
		{"destroy", defaultShutdownPhaseBudget, runner.Destroy},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), test.budget)
			defer cancel()
			_, err := test.invoke(ctx)
			require.NoError(t, err)
			remaining := <-peer.deadlines
			require.Greater(t, remaining, test.budget-time.Second)
			require.LessOrEqual(t, remaining, test.budget)
		})
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err = runner.Stop(ctx)
	require.NoError(t, err)
	require.LessOrEqual(t, <-peer.deadlines, time.Second, "a caller's shorter deadline remains authoritative")
}
