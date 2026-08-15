package orchestration

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	agentservices "github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	coreservices "github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type restartRuntimeClient struct {
	runtimev0.RuntimeClient
	stopCalls int
	stopErr   error
}

func (client *restartRuntimeClient) Stop(context.Context, *runtimev0.StopRequest, ...grpc.CallOption) (*runtimev0.StopResponse, error) {
	client.stopCalls++
	if client.stopErr != nil {
		return nil, client.stopErr
	}
	return &runtimev0.StopResponse{Status: &runtimev0.StopStatus{State: runtimev0.StopStatus_SUCCESS}}, nil
}

func runnerWithRuntimeClient(client runtimev0.RuntimeClient) *Runner {
	instance := &coreservices.Instance{
		Identity: &resources.ServiceIdentity{Module: "backend", Name: "auth-sidecar"},
	}
	instance.Runtime = &coreservices.RuntimeInstance{
		Instance: instance,
		Runtime:  &agentservices.RuntimeAgent{RuntimeClient: client},
	}
	return &Runner{instance: instance, stopped: make(chan struct{}, 1)}
}

func nativeMapping(name string, port uint16) *basev0.NetworkMapping {
	instance := resources.NewNetworkInstance("localhost", port)
	instance.Access = resources.NewNativeNetworkAccess()
	return &basev0.NetworkMapping{
		Endpoint:  &basev0.Endpoint{Name: name},
		Instances: []*basev0.NetworkInstance{instance},
	}
}

func TestBoundNativePortsDetectsHeldPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	held := ln.Addr().(*net.TCPAddr).Port

	// A free port: bind then release so nothing is listening on it.
	freeLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	free := freeLn.Addr().(*net.TCPAddr).Port
	require.NoError(t, freeLn.Close())

	runner := &Runner{networkMappings: []*basev0.NetworkMapping{
		nativeMapping("grpc", uint16(held)),
		nativeMapping("free", uint16(free)),
	}}

	require.Equal(t, []string{fmt.Sprintf("%d (grpc)", held)}, runner.boundNativePorts(context.Background()))
}

func TestBoundNativePortsToleratesNilMappings(t *testing.T) {
	runner := &Runner{networkMappings: []*basev0.NetworkMapping{
		nil,
		{Endpoint: nil},
		{Endpoint: &basev0.Endpoint{Name: "grpc"}, Instances: nil},
	}}
	require.Empty(t, runner.boundNativePorts(context.Background()))
}

func TestInitialPortGuardRejectsFirstInitAndSkipsRunningService(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	held := ln.Addr().(*net.TCPAddr).Port

	runner := &Runner{networkMappings: []*basev0.NetworkMapping{
		nativeMapping("http", uint16(held)),
	}}

	err = runner.checkInitialPortAvailability(context.Background())
	require.ErrorContains(t, err, fmt.Sprintf("port %d (http) already in use", held))

	// An initialized infrastructure runtime may already own this listener by
	// the time Start is called. Once the service is marked running, reloads
	// must not classify its own port as stale.
	runner.isStarted.Store(true)
	require.NoError(t, runner.checkInitialPortAvailability(context.Background()))
}

func TestStatusDiagnosticPreservesAgentMessage(t *testing.T) {
	require.Equal(t, "compile failed on line 12", statusDiagnostic("  compile failed on line 12  ", "fallback"))
	require.Equal(t, "fallback", statusDiagnostic("  ", "fallback"))
}

func TestRestartActionType(t *testing.T) {
	// A watch-triggered restart (START) must re-enter at configure (Init) so a
	// fresh compiler error gets re-attempted rather than relaunching the stale
	// binary.
	require.Equal(t, RuntimeInit, restartActionType(runtimev0.DesiredState_START))
	require.Equal(t, RuntimeInit, restartActionType(runtimev0.DesiredState_INIT))
	require.Equal(t, RuntimeLoad, restartActionType(runtimev0.DesiredState_LOAD))
}

func TestRestartSerializesEveryStartGeneration(t *testing.T) {
	client := &restartRuntimeClient{}
	runner := runnerWithRuntimeClient(client)
	var actions []Action
	var stopsBeforeCallback []int
	runner.callback = func(_ context.Context, action Action) error {
		actions = append(actions, action)
		stopsBeforeCallback = append(stopsBeforeCallback, client.stopCalls)
		return nil
	}

	require.NoError(t, runner.handleDesiredState(context.Background(), runtimev0.DesiredState_INIT))
	require.Empty(t, actions)
	require.Zero(t, client.stopCalls)

	runner.markStarted()
	require.NoError(t, runner.handleDesiredState(context.Background(), runtimev0.DesiredState_NOOP))
	require.Len(t, actions, 1)

	require.NoError(t, runner.handleDesiredState(context.Background(), runtimev0.DesiredState_START))
	require.Len(t, actions, 1)
	require.Equal(t, 1, client.stopCalls)

	runner.markStarted()
	require.NoError(t, runner.handleDesiredState(context.Background(), runtimev0.DesiredState_NOOP))
	require.Equal(t, []Action{
		{Service: "backend/auth-sidecar", Type: RuntimeInit},
		{Service: "backend/auth-sidecar", Type: RuntimeInit},
	}, actions)
	require.Equal(t, []int{1, 2}, stopsBeforeCallback)
}

func TestRestartDoesNotStartReplacementWhenStopFails(t *testing.T) {
	client := &restartRuntimeClient{stopErr: errors.New("runner still alive")}
	runner := runnerWithRuntimeClient(client)
	runner.markStarted()
	callbackCalls := 0
	runner.callback = func(context.Context, Action) error {
		callbackCalls++
		return nil
	}

	err := runner.handleDesiredState(context.Background(), runtimev0.DesiredState_START)

	require.ErrorContains(t, err, "stop before restart")
	require.ErrorContains(t, err, "runner still alive")
	require.Equal(t, 1, client.stopCalls)
	require.Zero(t, callbackCalls)
}
