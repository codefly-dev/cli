package portclaims

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
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
	pa, moved, err := first.Claim(context.Background(), RunKey("platform-core", a), "saas/store/tcp", ca, nil)
	require.NoError(t, err)
	require.False(t, moved)
	require.Equal(t, ca[0], pa)
	pb, moved, err := second.Claim(context.Background(), RunKey("platform-core", b), "saas/store/tcp", cb, nil)
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
	port, moved, err := l.Claim(context.Background(), RunKey("platform-core", "pc153-gate"), "records/records/connect", candidates, nil)
	require.NoError(t, err)
	require.True(t, moved)
	require.NotEqual(t, candidates[0], port)
	require.Contains(t, candidates, port)
}

// A run takes its own port again after a restart, whatever listens there: it
// may be its kept container, which the run's port guard decides about.
func TestARunKeepsItsOwnPortAcrossRestarts(t *testing.T) {
	candidates := []uint16{57760, 57770, 57780}
	first := testLedger(t, 100)
	first.held = func(port uint16) bool { return port == 57760 }
	port, _, err := first.Claim(context.Background(), RunKey("ws", "gate"), "saas/store/tcp", candidates, nil)
	require.NoError(t, err)
	require.Equal(t, uint16(57770), port)

	restarted := sibling(first, 300)
	restarted.alive = func(int) bool { return false }
	restarted.held = func(uint16) bool { return true }
	again, moved, err := restarted.Claim(context.Background(), RunKey("ws", "gate"), "saas/store/tcp", candidates, nil)
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
	_, _, err := stopped.Claim(context.Background(), RunKey("ws", "a"), "m/s/tcp", candidates, nil)
	require.NoError(t, err)

	other := sibling(stopped, 200)
	other.alive = func(p int) bool { return p == 200 }
	port, _, err := other.Claim(context.Background(), RunKey("ws", "b"), "m/s/tcp", candidates, nil)
	require.NoError(t, err)
	require.Equal(t, uint16(20010), port)

	now = now.Add(claimTTL + time.Hour)
	port, moved, err := other.Claim(context.Background(), RunKey("ws", "c"), "m/s/tcp", candidates, nil)
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
	port, moved, err := l.Claim(context.Background(), RunKey("ws", "gate"), Destination("saas/store/tcp", network.PortModeHost), candidates, nil)
	require.NoError(t, err)
	require.True(t, moved)

	require.Equal(t, fmt.Sprintf("localhost:%d", port), l.NativeFor(ctx, "ws", "saas", "store", "gate", endpoint).Address)
	require.Equal(t, network.NativeFor(ctx, "ws", "saas", "store", "other", endpoint).Address,
		l.NativeFor(ctx, "ws", "saas", "store", "other", endpoint).Address, "a scope with no claim reads the derived port")
}

// Runs that start at the same moment, each with its own ledger handle on one
// file, never take the same port: the pick and the record are one locked step.
func TestConcurrentRunsClaimDistinctPorts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	candidates := Candidates(context.Background(), "ws", "saas", "store", "", storeEndpoint, network.PortModeHost)
	const runs = 16
	ports := make([]uint16, runs)
	var wg sync.WaitGroup
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l := New(path)
			l.held = func(uint16) bool { return false }
			port, _, err := l.Claim(context.Background(), RunKey("ws", "scope-"+strconv.Itoa(i)), "saas/store/tcp", candidates, nil)
			require.NoError(t, err)
			ports[i] = port
		}(i)
	}
	wg.Wait()
	seen := map[uint16]bool{}
	for _, port := range ports {
		require.False(t, seen[port], "port %d handed to two runs", port)
		seen[port] = true
	}
}

// A derived port held by the endpoint's own kept container, which the ledger
// never recorded (a scope run before the ledger existed, or whose claim
// lapsed), is the endpoint's to keep, not a foreign holder to move away from.
func TestAPortHeldByTheEndpointsOwnContainerIsKept(t *testing.T) {
	candidates := []uint16{57760, 57770}
	l := testLedger(t, 100)
	l.held = func(uint16) bool { return true }
	port, moved, err := l.Claim(context.Background(), RunKey("ws", "gate"), "saas/store/tcp", candidates,
		func(port uint16) bool { return port == 57760 })
	require.NoError(t, err)
	require.False(t, moved)
	require.Equal(t, uint16(57760), port)
}

// An unreadable ledger is set aside rather than silently overwritten.
func TestAnUnreadableLedgerIsSetAside(t *testing.T) {
	l := testLedger(t, 100)
	require.NoError(t, os.WriteFile(l.path, []byte("{not json"), 0o600))
	port, _, err := l.Claim(context.Background(), RunKey("ws", "gate"), "m/s/tcp", []uint16{20000}, nil)
	require.NoError(t, err)
	require.Equal(t, uint16(20000), port)
	aside, err := filepath.Glob(l.path + ".corrupt-*")
	require.NoError(t, err)
	require.Len(t, aside, 1)
	kept, err := os.ReadFile(aside[0])
	require.NoError(t, err)
	require.Equal(t, "{not json", string(kept))
}

// No ledger (no home directory) claims nothing and reads the derived port.
func TestANilLedgerClaimsNothing(t *testing.T) {
	var l *Ledger
	port, moved, err := l.Claim(context.Background(), RunKey("ws", "gate"), "m/s/tcp", []uint16{20000, 20010}, nil)
	require.NoError(t, err)
	require.False(t, moved)
	require.Equal(t, uint16(20000), port)
	endpoint := &basev0.Endpoint{Name: "tcp", Api: "tcp"}
	require.Equal(t, network.NativeFor(context.Background(), "ws", "saas", "store", "gate", endpoint).Address,
		l.NativeFor(context.Background(), "ws", "saas", "store", "gate", endpoint).Address)
}
