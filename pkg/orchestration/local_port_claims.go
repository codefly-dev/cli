package orchestration

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"

	"github.com/codefly-dev/cli/pkg/portclaims"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// localPortClaims is a local run's side of the machine's port ledger
// (pkg/portclaims): which endpoints' ports this run has settled, so each is
// claimed once, before the first mapping that names it.
type localPortClaims struct {
	ledger *portclaims.Ledger

	mu sync.Mutex
	// settled is every endpoint destination whose port is decided: claimed
	// here, or pinned by the caller (Flow.WithPortOverrides), which wins.
	settled map[string]bool
	// failed stops claiming for the rest of the run once the ledger cannot be
	// used, so its endpoints keep one rule rather than a mix.
	failed bool
}

func (c *localPortClaims) pin(overrides map[string]uint16) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for destination := range overrides {
		c.settle(destination)
	}
}

func (c *localPortClaims) settle(destination string) {
	if c.settled == nil {
		c.settled = map[string]bool{}
	}
	c.settled[destination] = true
}

// localNetworkMappings is what a local run proposes for a service: core's
// allocation over its endpoints, once each endpoint's port is claimed.
//
// Both ways a local run names a service's address come through here — its own
// Init (gatherInitInputs) and a consumer deriving it before that Init
// (localProducerMappings) — so the first one claims the port and pins it in
// core's override seam, and every later one, the Init included, proposes the
// same address. A hot reload keeps it the same way pinAcceptedPorts does.
func (world *World) localNetworkMappings(ctx context.Context, identity *resources.ServiceIdentity, endpoints []*basev0.Endpoint, runtimeContext *basev0.RuntimeContext) ([]*basev0.NetworkMapping, error) {
	world.claimLocalPorts(ctx, identity, endpoints, runtimeContext)
	return world.LocalNetworkManager.GenerateNetworkMappings(ctx, world.Env.Runtime(), world.Workspace, identity, endpoints, runtimeContext)
}

// claimLocalPorts claims, for every endpoint not yet settled, the first port of
// its derived sequence that no other run on this machine claims and no other
// process holds, and pins it. Under --temporary-ports the kernel already hands
// out a free port, and without a ledger the derived port stands, as before.
//
// A derived port held only by the service's own kept container is the
// service's: it is claimed, and the first-Init guard adopts the container.
//
// A ledger that cannot be used leaves the derived ports in place for the rest
// of the run: the claim only prevents a collision the first-Init port guard
// still reports.
func (world *World) claimLocalPorts(ctx context.Context, identity *resources.ServiceIdentity, endpoints []*basev0.Endpoint, runtimeContext *basev0.RuntimeContext) {
	claims := &world.ports
	if claims.ledger == nil || claims.failed || world.temporaryPorts || world.LocalNetworkManager == nil || world.Workspace == nil || world.Env == nil {
		return
	}
	w := wool.Get(ctx).In("orchestration.claimLocalPorts", wool.ThisField(identity))
	mode := network.PortModeFor(runtimeContext.GetKind())
	scope := world.Env.NamingScope
	run := portclaims.RunKey(world.Workspace.Name, scope)

	adopted := world.adoptedContainerAddresses(ctx, world.containerRecoveryIdentity, identity)
	ours := func(port uint16) bool {
		p := strconv.Itoa(int(port))
		return adopted(net.JoinHostPort(loopbackIPv4, p)) || adopted(net.JoinHostPort(loopbackIPv6, p))
	}

	claims.mu.Lock()
	defer claims.mu.Unlock()
	overrides := map[string]uint16{}
	for _, endpoint := range endpoints {
		destination := resources.EndpointDestination(endpoint)
		if claims.failed || claims.settled[destination] {
			continue
		}
		claims.settle(destination)
		candidates := portclaims.Candidates(ctx, world.Workspace.Name, identity.Module, identity.Name, scope, endpoint, mode)
		port, moved, err := claims.ledger.Claim(ctx, run, portclaims.Destination(destination, mode), candidates, ours)
		if err != nil {
			claims.failed = true
			w.Warn(fmt.Sprintf("cannot claim host ports, so this run keeps its derived ports from %s on: %v", destination, err))
			continue
		}
		if moved {
			w.Warn(fmt.Sprintf(
				"%s: derived port %d is claimed by another run or held by another process, so this run serves it on %d and keeps that port for workspace %s, naming scope %q; an address resolved before this run (runnable bindings) named %d: regenerate it",
				destination, candidates[0], port, world.Workspace.Name, scope, candidates[0]))
		}
		overrides[destination] = port
	}
	if len(overrides) > 0 {
		world.LocalNetworkManager.WithPortOverrides(overrides)
	}
}
