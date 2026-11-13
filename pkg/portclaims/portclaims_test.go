package portclaims

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/network"
	"github.com/stretchr/testify/require"
)

var storeEndpoint = &basev0.Endpoint{Name: "tcp", Api: "tcp", Module: "saas", Service: "store"}

// collidingScopes finds two naming scopes whose derived port for the store's
// endpoint is the same — what happened to two agents' runs on one Mac.
func collidingScopes(t *testing.T, workspace string) (string, string) {
	t.Helper()
	seen := map[uint16]string{}
	for i := 0; i < 100000; i++ {
		scope := "scope-" + strconv.Itoa(i)
		port := Candidates(context.Background(), workspace, "saas", "store", scope, storeEndpoint, network.PortModeHost)[0]
		if other, ok := seen[port]; ok {
			return other, scope
		}
		seen[port] = scope
	}
	t.Fatal("no two scopes derive one port")
	return "", ""
}

func testLedger(t *testing.T, pid int) *Ledger {
	t.Helper()
	l := New(filepath.Join(t.TempDir(), "claims.json"))
	l.pid = pid
	l.alive = func(p int) bool { return p == pid }
	l.held = func(uint16) bool { return false }
	return l
}

func sibling(l *Ledger, pid int) *Ledger {
	other := New(l.path)
	other.pid, other.alive, other.held, other.now = pid, l.alive, l.held, l.now
	return other
}

// Two scopes of one service can derive the same port: the derivation alone
// gave both runs 57760. Claimed through the ledger they never share one.
func TestTwoScopesThatDeriveOnePortClaimDisjointPorts(t *testing.T) {
	ctx := context.Background()
	a, b := collidingScopes(t, "platform-core")
	ca := Candidates(ctx, "platform-core", "saas", "store", a, storeEndpoint, network.PortModeHost)
	cb := Candidates(ctx, "platform-core", "saas", "store", b, storeEndpoint, network.PortModeHost)
	require.Equal(t, ca[0], cb[0], "precondition: the two scopes derive one port")

	first := testLedger(t, 100)
	second := sibling(first, 200)
	second.alive = func(p int) bool { return p == 100 || p == 200 }
	pa, moved, err := first.Claim(RunKey("platform-core", a), "saas/store/tcp", ca)
	require.NoError(t, err)
	require.False(t, moved)
	require.Equal(t, ca[0], pa)
	pb, moved, err := second.Claim(RunKey("platform-core", b), "saas/store/tcp", cb)
	require.NoError(t, err)
	require.True(t, moved)
	require.NotEqual(t, pa, pb, "two live scopes were handed the same port")
	require.Equal(t, pa%10, pb%10, "a moved port keeps its API digit")
}

// A port another process listens on is skipped, not fatal.
func TestAHeldPortIsSkipped(t *testing.T) {
	ctx := context.Background()
	candidates := Candidates(ctx, "platform-core", "records", "records", "pc153-gate",
		&basev0.Endpoint{Name: "connect", Api: "connect"}, network.PortModeHost)
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(candidates[0]))))
	if err == nil {
		defer listener.Close()
	}
	// Bound by this test or by something else on this machine: held either way.
	require.True(t, Held(candidates[0]))

	l := New(filepath.Join(t.TempDir(), "claims.json"))
	port, moved, err := l.Claim(RunKey("platform-core", "pc153-gate"), "records/records/connect", candidates)
	require.NoError(t, err)
	require.True(t, moved)
	require.NotEqual(t, candidates[0], port)
	require.Contains(t, candidates, port)
	require.False(t, Held(port), "the claimed port is free")
}

// A run takes its own port again after a restart, whatever listens there: it
// may be its kept container, which the run's port guard decides about.
func TestARunKeepsItsOwnPortAcrossRestarts(t *testing.T) {
	candidates := []uint16{57760, 57770, 57780}
	first := testLedger(t, 100)
	first.held = func(port uint16) bool { return port == 57760 }
	port, _, err := first.Claim(RunKey("ws", "gate"), "saas/store/tcp", candidates)
	require.NoError(t, err)
	require.Equal(t, uint16(57770), port)

	restarted := sibling(first, 300)
	restarted.alive = func(int) bool { return false }
	restarted.held = func(uint16) bool { return true }
	again, moved, err := restarted.Claim(RunKey("ws", "gate"), "saas/store/tcp", candidates)
	require.NoError(t, err)
	require.False(t, moved)
	require.Equal(t, port, again)
}

// A stopped run's ports stay reserved against other runs until its claim
// lapses, so a scope that stops is not robbed by the next one to start.
func TestAStoppedRunsClaimBlocksUntilItLapses(t *testing.T) {
	candidates := []uint16{20000, 20010}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	stopped := testLedger(t, 100)
	stopped.now = func() time.Time { return now }
	_, _, err := stopped.Claim(RunKey("ws", "a"), "m/s/tcp", candidates)
	require.NoError(t, err)

	other := sibling(stopped, 200)
	other.alive = func(p int) bool { return p == 200 }
	port, _, err := other.Claim(RunKey("ws", "b"), "m/s/tcp", candidates)
	require.NoError(t, err)
	require.Equal(t, uint16(20010), port)

	now = now.Add(claimTTL + time.Hour)
	port, moved, err := other.Claim(RunKey("ws", "c"), "m/s/tcp", candidates)
	require.NoError(t, err)
	require.False(t, moved)
	require.Equal(t, uint16(20000), port, "a lapsed claim of a dead run frees its port")
}

// The static address readers (codefly endpoint, show network, runnable
// bindings) read a run's claim, so they agree with the run that moved.
func TestNativeForReadsTheRunsClaim(t *testing.T) {
	ctx := context.Background()
	endpoint := &basev0.Endpoint{Name: "tcp", Api: "tcp"}
	derived := network.NativeFor(ctx, "ws", "saas", "store", "gate", endpoint)
	l := testLedger(t, 100)
	l.held = func(port uint16) bool { return uint32(port) == derived.Port }
	candidates := Candidates(ctx, "ws", "saas", "store", "gate", storeEndpoint, network.PortModeHost)
	require.Equal(t, derived.Port, uint32(candidates[0]))
	port, moved, err := l.Claim(RunKey("ws", "gate"), Destination("saas/store/tcp", network.PortModeHost), candidates)
	require.NoError(t, err)
	require.True(t, moved)

	require.Equal(t, fmt.Sprintf("localhost:%d", port), l.NativeFor(ctx, "ws", "saas", "store", "gate", endpoint).Address)
	require.Equal(t, network.NativeFor(ctx, "ws", "saas", "store", "other", endpoint).Address,
		l.NativeFor(ctx, "ws", "saas", "store", "other", endpoint).Address, "a scope with no claim reads the derived port")
}
