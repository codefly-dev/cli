package orchestration

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

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

func TestRestartWaitsForFirstSuccessfulStart(t *testing.T) {
	runner := &Runner{started: make(chan struct{})}
	ready := make(chan bool, 1)
	go func() {
		ready <- runner.waitForFirstStart(context.Background())
	}()

	require.Never(t, func() bool { return len(ready) > 0 }, 50*time.Millisecond, 5*time.Millisecond)

	runner.markStarted()
	require.True(t, <-ready)
}
