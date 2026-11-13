// Package portclaims is this machine's record of which local run binds which
// derived host port, so several `codefly run`s in different naming scopes can
// run side by side.
//
// A local run derives each endpoint's host port from its identity (core's
// network.ToNamedPort over workspace, module, service, endpoint and naming
// scope). That keeps an endpoint's address stable across restarts, but it is a
// hash into about 6,450 slots per API kind: two scopes of one service collide by
// chance, two compositions of ~65 endpoints share a port about every other
// time, and nothing about the hash knows that another run, or any other
// process, already listens there. Each run then refused at its first Init,
// naming the holder, and the only remedy was to pick another scope and retry.
//
// The ledger closes that. An endpoint's port is the first candidate of its own
// deterministic sequence (the derived port first, then re-derivations of it)
// that no other run claims and nothing listens on, and the choice is written
// down under a file lock, so two runs starting at the same moment cannot take
// the same one. The run's own claim is kept: the next run of the same
// workspace and scope takes it again first, whoever listens there, because the
// listener may be that scope's kept container or its own leftover, which the
// run's first-Init guard tells apart and reports. Another run's claim blocks
// while that run is alive and for claimTTL after it was last used, so a scope
// that stops is not robbed of its ports by the next one to start.
package portclaims

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/gofrs/flock"
	"github.com/shirou/gopsutil/v3/process"
)

// PathEnv overrides where the ledger lives. Ports are the host's, so the
// default is under the user's home and does not follow CODEFLY_HOME, which a
// CI run isolates per invocation.
const PathEnv = "CODEFLY_PORT_CLAIMS"

// claimTTL is how long a stopped run keeps its ports reserved against other
// runs. Every run refreshes its claims, so only a scope unused this long loses
// them.
const claimTTL = 7 * 24 * time.Hour

// candidateCount bounds the re-derivations tried after the derived port.
const candidateCount = 64

// lockTimeout bounds the wait for another run's claim to finish.
const lockTimeout = 30 * time.Second

// Claim is one port's owner: the run (workspace and naming scope), the
// endpoint, the process that last claimed it and when.
type Claim struct {
	Run         string    `json:"run"`
	Destination string    `json:"destination"`
	PID         int       `json:"pid"`
	Seen        time.Time `json:"seen"`
}

// Ledger is the claims file and the probes it decides with.
type Ledger struct {
	path  string
	mu    sync.Mutex
	now   func() time.Time
	pid   int
	alive func(pid int) bool
	held  func(port uint16) bool
}

// Default is the machine's ledger: $CODEFLY_PORT_CLAIMS, else
// ~/.codefly/ports/claims.json.
func Default() *Ledger {
	if path := os.Getenv(PathEnv); path != "" {
		return New(path)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return New(filepath.Join(home, ".codefly", "ports", "claims.json"))
}

// New is a ledger at path, deciding with the real process table and sockets.
func New(path string) *Ledger {
	return &Ledger{path: path, now: time.Now, pid: os.Getpid(), alive: pidAlive, held: Held}
}

// RunKey names one run's port space: its workspace and naming scope.
func RunKey(workspace, namingScope string) string {
	return workspace + "#" + namingScope
}

// Destination names one endpoint in one port mode, the axis core also folds
// into the derived port (network.PortModeFor).
func Destination(endpointDestination, mode string) string {
	if mode == network.PortModeHost {
		return endpointDestination
	}
	return endpointDestination + "@" + mode
}

// Candidates is an endpoint's port sequence: the derived port exactly as core
// derives it (network.NativeFor's naming-scope folding, network.ToNamedPort),
// then re-derivations of it, which keep its API digit.
func Candidates(ctx context.Context, workspace, module, service, namingScope string, endpoint *basev0.Endpoint, mode string) []uint16 {
	name := endpoint.GetName()
	if namingScope != "" {
		name = fmt.Sprintf("%s-%s", name, namingScope)
	}
	seen := map[uint16]bool{}
	candidates := make([]uint16, 0, candidateCount)
	for i := 0; i < candidateCount; i++ {
		salted := name
		if i > 0 {
			salted = name + "~" + strconv.Itoa(i)
		}
		port := network.ToNamedPort(ctx, workspace, module, service, salted, endpoint.GetApi(), mode)
		if !seen[port] {
			seen[port] = true
			candidates = append(candidates, port)
		}
	}
	return candidates
}

// Claim returns the port run binds for destination: its own earlier claim,
// else the first candidate no other live or recent claim names and nothing
// listens on, which it records. moved reports that this call chose a port
// other than the derived one (candidates[0]). With every candidate taken it
// returns the derived port unrecorded, so the run's port guard names the
// holder.
func (l *Ledger) Claim(run, destination string, candidates []uint16) (port uint16, moved bool, err error) {
	if len(candidates) == 0 {
		return 0, false, errors.New("no candidate port")
	}
	err = l.update(func(claims map[uint16]Claim) bool {
		if owned, ok := own(claims, run, destination); ok {
			port = owned
		} else if port = free(claims, candidates, l.held); port == 0 {
			port = candidates[0]
			return false
		} else {
			moved = port != candidates[0]
		}
		claims[port] = Claim{Run: run, Destination: destination, PID: l.pid, Seen: l.now().UTC()}
		return true
	})
	if err != nil {
		return candidates[0], false, err
	}
	return port, moved, nil
}

// free is the first candidate no claim names and nothing listens on, or 0.
func free(claims map[uint16]Claim, candidates []uint16, held func(uint16) bool) uint16 {
	for _, port := range candidates {
		if _, claimed := claims[port]; !claimed && !held(port) {
			return port
		}
	}
	return 0
}

// Lookup is run's recorded port for destination, read without claiming.
func (l *Ledger) Lookup(run, destination string) (uint16, bool) {
	claims, err := l.read()
	if err != nil {
		return 0, false
	}
	return own(claims, run, destination)
}

// NativeFor is network.NativeFor with the run's claim when it has one: the
// address a run of that workspace and scope binds, read without a live run.
func (l *Ledger) NativeFor(ctx context.Context, workspace, module, service, namingScope string, endpoint *basev0.Endpoint) *basev0.NetworkInstance {
	stamped := &basev0.Endpoint{Module: module, Service: service, Name: endpoint.GetName(), Api: endpoint.GetApi()}
	if port, ok := l.Lookup(RunKey(workspace, namingScope), Destination(resources.EndpointDestination(stamped), network.PortModeHost)); ok {
		return network.Native(endpoint, port)
	}
	return network.NativeFor(ctx, workspace, module, service, namingScope, endpoint)
}

func own(claims map[uint16]Claim, run, destination string) (uint16, bool) {
	ports := make([]uint16, 0, 1)
	for port, claim := range claims {
		if claim.Run == run && claim.Destination == destination {
			ports = append(ports, port)
		}
	}
	if len(ports) == 0 {
		return 0, false
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	return ports[0], true
}

// update runs change over the claims under the file lock, dropping claims that
// lapsed, and writes them back when change reports it recorded one.
func (l *Ledger) update(change func(map[uint16]Claim) bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return fmt.Errorf("port claims: %w", err)
	}
	lock := flock.New(l.path + ".lock")
	ctx, cancel := context.WithTimeout(context.Background(), lockTimeout)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, 20*time.Millisecond)
	if err != nil || !locked {
		return fmt.Errorf("port claims: cannot lock %s: %v", l.path, err)
	}
	defer func() { _ = lock.Unlock() }()
	claims, err := l.read()
	if err != nil {
		// An unreadable ledger is rewritten rather than trusted or fatal: the
		// claims in it protect only against a collision the port guard still
		// reports.
		claims = map[uint16]Claim{}
	}
	for port, claim := range claims {
		if l.now().Sub(claim.Seen) > claimTTL && !l.alive(claim.PID) {
			delete(claims, port)
		}
	}
	if !change(claims) {
		return nil
	}
	return l.write(claims)
}

func (l *Ledger) read() (map[uint16]Claim, error) {
	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[uint16]Claim{}, nil
	}
	if err != nil {
		return nil, err
	}
	claims := map[uint16]Claim{}
	if err := json.Unmarshal(data, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

func (l *Ledger) write(claims map[uint16]Claim) error {
	data, err := json.MarshalIndent(claims, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(l.path), ".claims-*")
	if err != nil {
		return fmt.Errorf("port claims: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("port claims: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("port claims: %w", err)
	}
	return os.Rename(tmp.Name(), l.path)
}

func pidAlive(pid int) bool {
	if pid <= 0 || pid > math.MaxInt32 {
		return false
	}
	exists, err := process.PidExists(int32(pid))
	return err == nil && exists
}

// Held reports whether something already serves port on this host: anything
// answering on either loopback family, or a wildcard bind the kernel refuses.
func Held(port uint16) bool {
	p := strconv.Itoa(int(port))
	for _, host := range []string{"127.0.0.1", "::1"} {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, p), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
	}
	listener, err := net.Listen("tcp", ":"+p)
	if err != nil {
		return true
	}
	_ = listener.Close()
	return false
}
