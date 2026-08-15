package orchestration

import (
	"context"
	"fmt"
	"net"
	"testing"

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

func TestNativeLivenessReportsReachability(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	up := ln.Addr().(*net.TCPAddr).Port

	// A port nothing listens on.
	downLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	down := downLn.Addr().(*net.TCPAddr).Port
	require.NoError(t, downLn.Close())

	// A dead endpoint alongside a live one is still reachable overall.
	runner := &Runner{networkMappings: []*basev0.NetworkMapping{
		nativeMapping("postgres", uint16(down)),
		nativeMapping("grpc", uint16(up)),
	}}
	native, reachable := runner.nativeLiveness(context.Background())
	require.Len(t, native, 2)
	require.True(t, reachable)

	// Every native endpoint down: has endpoints to probe, none reachable.
	runner = &Runner{networkMappings: []*basev0.NetworkMapping{
		nativeMapping("postgres", uint16(down)),
	}}
	native, reachable = runner.nativeLiveness(context.Background())
	require.Len(t, native, 1)
	require.False(t, reachable)

	// No native endpoints at all: nothing to probe.
	runner = &Runner{networkMappings: []*basev0.NetworkMapping{
		nil,
		{Endpoint: nil},
	}}
	native, _ = runner.nativeLiveness(context.Background())
	require.Empty(t, native)
}

func TestLivenessTrackerDeclaresDeathOnlyAfterHealthyThenStreak(t *testing.T) {
	var tracker livenessTracker

	// Nothing to probe never arms the tracker.
	require.False(t, tracker.observe(false, false))

	// Unreachable before ever being healthy is a slow start, not a death.
	for i := 0; i < livenessFailureThreshold+2; i++ {
		require.False(t, tracker.observe(true, false))
	}

	// Once healthy, the failure streak must reach the threshold to declare death.
	require.False(t, tracker.observe(true, true))
	for i := 1; i < livenessFailureThreshold; i++ {
		require.False(t, tracker.observe(true, false))
	}
	require.True(t, tracker.observe(true, false))
}

func TestLivenessTrackerResetsStreakOnRecovery(t *testing.T) {
	var tracker livenessTracker

	require.False(t, tracker.observe(true, true))
	// A blip shorter than the threshold followed by recovery must not fire.
	for i := 1; i < livenessFailureThreshold; i++ {
		require.False(t, tracker.observe(true, false))
	}
	require.False(t, tracker.observe(true, true))
	// After recovery the streak is clear, so a single miss does not fire.
	require.False(t, tracker.observe(true, false))
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
