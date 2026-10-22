package orchestration

import (
	"context"
	"net"
	"sort"
	"strconv"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// LOOPBACK, THE LAPTOP'S STAND-IN FOR THE MESH.
//
// In a cell the mesh decides who reaches a workload: every edge is a declared
// principal, and a port nobody is admitted to is a port nothing reaches. A
// native run has no mesh, so its stand-in for that rule is reach itself:
// every service but the host's gateway listens on loopback, where nothing off
// this machine can dial it, and the gateway alone is the one listener a
// browser or a sibling machine may reach. The CLI does not open a service's
// socket; what it owns is the address it hands the service to listen on — its
// own CODEFLY__ENDPOINT__ carrier, which a native run writes as a loopback
// address (core's network.Native) — and the service is expected to listen on
// exactly that host. This file is the check behind that expectation: once a
// service is Running, each of its native ports is dialled once on each of
// this machine's non-loopback addresses, and an answer is warned about,
// naming the rule. A warning rather than a refusal, for now: agents that
// still bind every interface sit outside the CLI, and a refusal would stop a
// run on code it does not own; it becomes a refusal once every agent listens
// on its carrier's host.

// loopbackProbeTimeout bounds one connect attempt. The check is cheap and
// bounded: one attempt per address and port, never retried.
const loopbackProbeTimeout = 300 * time.Millisecond

// OffLoopbackAnswers reports the non-loopback addresses of this machine on
// which a native service answers: for each native-access port in its network
// mappings, one connect attempt per non-loopback unicast address of the
// machine's interfaces. An address that answers is one a sibling machine
// could reach too. Sorted, each once.
func OffLoopbackAnswers(ctx context.Context, mappings []*basev0.NetworkMapping, timeout time.Duration) []string {
	return offLoopbackAnswers(ctx, mappings, machineAddresses(), timeout)
}

func offLoopbackAnswers(ctx context.Context, mappings []*basev0.NetworkMapping, addresses []net.IP, timeout time.Duration) []string {
	if len(addresses) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var answers []string
	dialer := &net.Dialer{Timeout: timeout}
	for _, mapping := range mappings {
		if mapping == nil || mapping.GetEndpoint() == nil {
			continue
		}
		instance := resources.FilterNetworkInstance(ctx, mapping.GetInstances(), resources.NewNativeNetworkAccess())
		if instance == nil || instance.GetPort() == 0 {
			continue
		}
		port := strconv.FormatUint(uint64(instance.GetPort()), 10)
		for _, address := range addresses {
			target := net.JoinHostPort(address.String(), port)
			if seen[target] {
				continue
			}
			seen[target] = true
			conn, err := dialer.DialContext(ctx, "tcp", target)
			if err != nil {
				continue
			}
			_ = conn.Close()
			answers = append(answers, target)
		}
	}
	sort.Strings(answers)
	return answers
}

// machineAddresses lists this machine's non-loopback unicast addresses: what
// a sibling machine on the same network could dial.
func machineAddresses() []net.IP {
	interfaces, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var addresses []net.IP
	for _, address := range interfaces {
		network, ok := address.(*net.IPNet)
		if !ok || network.IP == nil || network.IP.IsLoopback() || !network.IP.IsGlobalUnicast() && !network.IP.IsPrivate() {
			continue
		}
		addresses = append(addresses, network.IP)
	}
	return addresses
}

// checkLoopback warns, once a native service is Running, when it answers off
// loopback: in a native run only the host's gateway may. It runs on its own
// goroutine and never blocks the run.
func (flow *Flow) checkLoopback(service string) {
	if flow == nil || flow.world == nil || flow.world.SharedState == nil {
		return
	}
	loaded, err := flow.ServiceFromUnique(service)
	if err != nil || loaded == nil || flow.runtimeContextFor(loaded) != resources.RuntimeContextNative {
		return
	}
	if flow.world.Env != nil && flow.world.Env.Host != nil && flow.world.Env.Host.Gateway == service {
		return
	}
	mappings, ok := flow.world.SharedState.GetNetworkMappingsFromUnique(service)
	if !ok {
		return
	}
	agent := ""
	if loaded.Agent != nil {
		agent = loaded.Agent.Name
	}
	go func() {
		ctx := context.Background()
		answers := OffLoopbackAnswers(ctx, mappings, loopbackProbeTimeout)
		if len(answers) == 0 {
			return
		}
		wool.Get(ctx).In("Flow.checkLoopback").Warn(
			"a native service answers off loopback; in a native run only the host gateway may be reachable off loopback — the service must listen on the host its own CODEFLY__ENDPOINT__ carrier names",
			wool.Field("service", service), wool.Field("agent", agent), wool.Field("answers", answers))
	}()
}
