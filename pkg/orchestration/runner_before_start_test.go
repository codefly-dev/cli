package orchestration

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"

	agentservices "github.com/codefly-dev/core/agents/services"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// A controlled peer on a real socket observes whether Start crossed the agent
// boundary. Its response qualifies CLI sequencing, not host admission itself.
type beforeStartPeer struct {
	realAgent
	starts     atomic.Int32
	admissions atomic.Int32
}

func (peer *beforeStartPeer) Start(context.Context, *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	count := peer.starts.Add(1)
	if peer.admissions.Load() < count {
		return nil, errors.New("Start arrived without its preceding admission")
	}
	return &runtimev0.StartResponse{Status: &runtimev0.StartStatus{State: runtimev0.StartStatus_STARTED}}, nil
}

func beforeStartRunner(t *testing.T) (*Runner, *World, *beforeStartPeer) {
	t.Helper()
	peer := &beforeStartPeer{realAgent: realAgent{mode: realAgentEcho}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	runtimev0.RegisterRuntimeServer(server, peer)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	runner, world := gatewayRunner(t, agentservices.NewRuntimeAgentClient(conn))
	_, err = runner.Init(context.Background())
	require.NoError(t, err)
	return runner, world, peer
}

func TestBeforeRuntimeStartPrecedesEveryAgentStart(t *testing.T) {
	runner, world, peer := beforeStartRunner(t)
	world.BeforeRuntimeStart = func(ctx context.Context, service *resources.Service) error {
		require.Same(t, runner.instance.Service, service)
		require.NoError(t, ctx.Err())
		peer.admissions.Add(1)
		return nil
	}
	for range 2 {
		output, err := runner.Start(context.Background())
		require.NoError(t, err)
		require.NotNil(t, output)
	}
	require.EqualValues(t, 2, peer.starts.Load())
	require.EqualValues(t, 2, peer.admissions.Load())
}

func TestBeforeRuntimeStartRefusalAndCancellationPreventAgentStart(t *testing.T) {
	for _, scenario := range []string{"refused", "cancelled before hook", "cancelled during hook"} {
		t.Run(scenario, func(t *testing.T) {
			runner, world, peer := beforeStartRunner(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			refusal := errors.New("manifest was not applied")
			calls := 0
			world.BeforeRuntimeStart = func(context.Context, *resources.Service) error {
				calls++
				if scenario == "refused" {
					return refusal
				}
				cancel()
				return nil
			}
			if scenario == "cancelled before hook" {
				cancel()
			}
			_, err := runner.Start(ctx)
			if scenario == "refused" {
				require.ErrorIs(t, err, refusal)
			} else {
				require.ErrorIs(t, err, context.Canceled)
			}
			if scenario == "cancelled before hook" {
				require.Zero(t, calls)
			} else {
				require.Equal(t, 1, calls)
			}
			require.Zero(t, peer.starts.Load())
			require.False(t, runner.isStarted.Load())
		})
	}
}
