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
	fail       atomic.Bool
}

func (peer *beforeStartPeer) Start(context.Context, *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	count := peer.starts.Add(1)
	if peer.admissions.Load() < count {
		return nil, errors.New("Start arrived without its preceding admission")
	}
	if peer.fail.Load() {
		return nil, errors.New("controlled start failure")
	}
	return &runtimev0.StartResponse{Status: &runtimev0.StartStatus{State: runtimev0.StartStatus_STARTED}}, nil
}

func TestRuntimeStartCompletionReportsActualSuccess(t *testing.T) {
	for _, scenario := range []string{"success", "agent failure", "admission failure", "cancel after admission"} {
		t.Run(scenario, func(t *testing.T) {
			runner, world, peer := beforeStartRunner(t)
			peer.fail.Store(scenario == "agent failure")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var outcomes []bool
			world.BeginRuntimeStart = func(context.Context, *resources.Service) (*RuntimeStartAttempt, error) {
				finish := func(started bool) { outcomes = append(outcomes, started) }
				if scenario == "admission failure" {
					return &RuntimeStartAttempt{Complete: finish, Admit: func(context.Context) error { return errors.New("not applied") }}, nil
				}
				return &RuntimeStartAttempt{Complete: finish, Admit: func(context.Context) error {
					peer.admissions.Add(1)
					if scenario == "cancel after admission" {
						cancel()
					}
					return nil
				}}, nil
			}
			_, err := runner.Start(ctx)
			if scenario == "success" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Equal(t, []bool{scenario == "success"}, outcomes)
		})
	}
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
	world.BeginRuntimeStart = func(ctx context.Context, service *resources.Service) (*RuntimeStartAttempt, error) {
		require.Same(t, runner.instance.Service, service)
		require.NoError(t, ctx.Err())
		return &RuntimeStartAttempt{Admit: func(context.Context) error { peer.admissions.Add(1); return nil }}, nil
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
			world.BeginRuntimeStart = func(context.Context, *resources.Service) (*RuntimeStartAttempt, error) {
				calls++
				if scenario == "refused" {
					return &RuntimeStartAttempt{Admit: func(context.Context) error { return refusal }}, nil
				}
				cancel()
				return nil, nil
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
			require.Equal(t, 1, calls)
			require.Zero(t, peer.starts.Load())
			require.False(t, runner.isStarted.Load())
		})
	}
}

func TestRuntimeStartPreparationFailureCompletesAttempt(t *testing.T) {
	for _, scenario := range []string{"output environment", "cancel during overrides"} {
		t.Run(scenario, func(t *testing.T) {
			runner, world, peer := beforeStartRunner(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var outcomes []bool
			world.BeginRuntimeStart = func(context.Context, *resources.Service) (*RuntimeStartAttempt, error) {
				return &RuntimeStartAttempt{
					Complete: func(started bool) { outcomes = append(outcomes, started) },
					Admit:    func(context.Context) error { peer.admissions.Add(1); return nil },
				}, nil
			}
			if scenario == "output environment" {
				runner.outputEnv = t.TempDir() // A directory cannot be an output file.
			} else {
				world.RuntimeOverrides = func(*resources.Service) map[string]string {
					cancel()
					return nil
				}
			}
			_, err := runner.Start(ctx)
			require.Error(t, err)
			require.Equal(t, []bool{false}, outcomes)
			require.Zero(t, peer.admissions.Load())
			require.Zero(t, peer.starts.Load())
		})
	}
}
