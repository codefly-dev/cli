package orchestration

import (
	"context"
	"math"
	"net"
	"strconv"
	"sync"

	basev1 "github.com/codefly-dev/core/generated/go/codefly/base/v1"
	"github.com/codefly-dev/core/resources"
)

// portReservations is the run holding each host port it chose for a service,
// from the moment it chose it until that service's first Init.
//
// Choosing a port does not bind it. A local run settles a service's port when
// the first mapping names it — often a consumer deriving the address long
// before the producer starts — by claiming the derived port in the machine's
// ledger, or, under --temporary-ports, by asking the kernel for a free one and
// closing the probe. Either way the port is then only written down: in the
// ledger, which other codefly runs read, and in core's allocation table, which
// this process reads. The kernel reads neither. Every agent the run starts
// serves its own gRPC on a loopback port the kernel assigns (agents.Serve
// binds port 0), the kernel's range overlaps the derived ports and is the
// whole of the temporary ones, and so a chosen port nothing held was handed to
// another service's agent of the same run before its own service bound it:
// "port 62853 (grpc) held by pid … (…/python-fastapi__0.0.122) already in use".
//
// So the run binds what it chose. A listener on each loopback family keeps the
// kernel from assigning the port to anyone, and makes it read as held to
// another run's probe. It is released at the service's first Init, just before
// the guard that checks the port is free and the agent that binds it — an
// infrastructure agent binds during Init, so nothing later would do — and never
// taken again: from then on the port is the service's, restarts included.
//
// What is left is the hand-over itself: between that release and the
// service's own bind the port is free again, for as long as the service takes
// to start. A reservation cannot be passed to a process the run does not
// write, so that window is narrowed to one service's start rather than closed.
//
// Loopback only: the run serves nothing here, so it listens on no interface
// another machine can reach. A port that cannot be bound is already held — by
// the service's own kept container, or by a stranger the first-Init guard
// names — and is left as it is.
type portReservations struct {
	mu sync.Mutex
	// held is the listeners on each reserved port.
	held map[uint16][]net.Listener
	// handedOver is every port whose service reached its first Init.
	handedOver map[uint16]bool
	// closed is set when the run stops: nothing is reserved after that.
	closed bool
}

func (r *portReservations) hold(port uint16) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || port == 0 || r.handedOver[port] || len(r.held[port]) > 0 {
		return
	}
	for _, host := range []string{loopbackIPv4, loopbackIPv6} {
		listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
		if err != nil {
			continue
		}
		if r.held == nil {
			r.held = map[uint16][]net.Listener{}
		}
		r.held[port] = append(r.held[port], listener)
	}
}

func (r *portReservations) handOver(port uint16) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handedOver == nil {
		r.handedOver = map[uint16]bool{}
	}
	r.handedOver[port] = true
	r.close(port)
}

// releaseAll lets go of every port still held and reserves nothing afterwards:
// the run is stopping, and a service that never reached its Init binds nothing.
func (r *portReservations) releaseAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	for port := range r.held {
		r.close(port)
	}
}

func (r *portReservations) close(port uint16) {
	for _, listener := range r.held[port] {
		_ = listener.Close()
	}
	delete(r.held, port)
}

// nativePorts is the host port of every mapping a local process binds: the
// native instance of each, external addresses left out.
func nativePorts(ctx context.Context, mappings []*basev1.NetworkMapping) []uint16 {
	var ports []uint16
	for _, mapping := range mappings {
		if mapping == nil || mapping.Endpoint == nil || resources.IsExternalEndpoint(mapping.Endpoint) {
			continue
		}
		instance := resources.FilterNetworkInstance(ctx, mapping.Instances, resources.NewNativeNetworkAccess())
		if instance == nil || !isLoopbackHostname(instance.GetHostname()) {
			continue
		}
		// A port outside the TCP range names nothing a process can bind, so
		// there is nothing to hold; truncating it would hold a stranger's port.
		port := instance.GetPort()
		if port == 0 || port > math.MaxUint16 {
			continue
		}
		ports = append(ports, uint16(port))
	}
	return ports
}

// holdProposedPorts reserves the ports mappings propose (portReservations).
func (world *World) holdProposedPorts(ctx context.Context, mappings []*basev1.NetworkMapping) {
	for _, port := range nativePorts(ctx, mappings) {
		world.ports.reservations.hold(port)
	}
}

// releaseProposedPorts hands the ports mappings propose over to their service,
// which is about to initialize.
func (world *World) releaseProposedPorts(ctx context.Context, mappings []*basev1.NetworkMapping) {
	for _, port := range nativePorts(ctx, mappings) {
		world.ports.reservations.handOver(port)
	}
}
