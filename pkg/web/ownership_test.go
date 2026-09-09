package web

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// A second codefly for the same workspace derives the identical control
// address. It has to discover that before it owns anything else, and the
// message has to say what to do about it.
func TestListenRefusesAnAlreadyOwnedControlAddress(t *testing.T) {
	workspace := &resources.Workspace{Name: "control-ownership-test"}
	owner := newTestServer(t, workspace)
	require.NoError(t, owner.Listen())
	defer owner.Close()

	loser := newTestServer(t, workspace)
	err := loser.Listen()

	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot own the codefly control server")
	require.Contains(t, err.Error(), "--naming-scope")
	loser.Close()
}

// An unrelated process squatting on the derived port is the same failure, and
// must not be mistaken for a codefly this run may drive.
func TestListenRefusesAForeignListenerOnTheControlPort(t *testing.T) {
	workspace := &resources.Workspace{Name: "control-foreign-listener-test"}
	server := newTestServer(t, workspace)

	squatter, err := net.Listen("tcp", server.server.Address())
	require.NoError(t, err)
	defer squatter.Close()

	err = server.Listen()
	require.ErrorContains(t, err, "cannot own the codefly control server")
	require.NotContains(t, err.Error(), "codefly is already serving",
		"a plain listener on the derived port was reported as a codefly to attach to")
}

// A run that aborts between claiming the control address and serving it must
// leave the address free, or the next run inherits a phantom owner.
func TestCloseReleasesAClaimedButUnservedAddress(t *testing.T) {
	workspace := &resources.Workspace{Name: "control-release-test"}
	first := newTestServer(t, workspace)
	require.NoError(t, first.Listen())
	first.Close()

	second := newTestServer(t, workspace)
	require.NoError(t, second.Listen())
	second.Close()
}

// Start binds for callers that own nothing else, and the claim Listen already
// made is the one Start serves — not a second bind that would fail.
func TestStartServesTheAddressListenAlreadyClaimed(t *testing.T) {
	server := newTestServer(t, &resources.Workspace{Name: "control-start-test"})
	require.NoError(t, server.Listen())

	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- server.Start(ctx) }()

	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", server.server.Address(), 100*time.Millisecond)
		if err != nil {
			return false
		}
		return conn.Close() == nil
	}, 5*time.Second, 20*time.Millisecond, "Start never served the claimed control address")

	cancel()
	select {
	case err := <-served:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after cancellation")
	}
}

func newTestServer(t *testing.T, workspace *resources.Workspace) *CodeflyServer {
	t.Helper()
	server, err := NewServer(ServerData{Workspace: workspace})
	require.NoError(t, err)
	return server
}

// Listen, `go Start`, `defer Close` is the pattern this API invites, and it has
// Close releasing the same listener the serving goroutine is releasing. Run
// under -race, an unguarded listener field fails here.
func TestCloseIsSafeWhileStartIsServing(t *testing.T) {
	server := newTestServer(t, &resources.Workspace{Name: "control-close-race-test"})
	require.NoError(t, server.Listen())

	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- server.Start(ctx) }()
	defer server.Close()

	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", server.server.Address(), 100*time.Millisecond)
		if err != nil {
			return false
		}
		return conn.Close() == nil
	}, 5*time.Second, 20*time.Millisecond, "Start never served the claimed control address")

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		server.Close()
	}()
	cancel()
	<-closed
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after a concurrent Close and cancellation")
	}
}
