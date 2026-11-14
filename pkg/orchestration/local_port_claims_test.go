package orchestration

import (
	"context"
	"maps"
	"net"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/portclaims"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
	dockercontainer "github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/require"
)

func claimsTestEndpoint(t *testing.T) *basev0.Endpoint {
	t.Helper()
	declaration := resources.Endpoint{Name: "tcp", API: "tcp", Visibility: resources.VisibilityPrivate, Module: "saas", Service: "store"}
	endpoint, err := declaration.Proto()
	require.NoError(t, err)
	return endpoint
}

// claimsTestWorld is a local run's world in one naming scope, claiming through
// ledger when it is not nil.
func claimsTestWorld(t *testing.T, scope string, ledger *portclaims.Ledger) *World {
	t.Helper()
	manager, err := network.NewRuntimeManager(context.Background(), nil)
	require.NoError(t, err)
	world := &World{
		Workspace:           &resources.Workspace{Name: "platform-core"},
		Env:                 &environments.Environment{Name: "local", NamingScope: scope},
		LocalNetworkManager: manager,
	}
	world.ports.ledger = ledger
	return world
}

func proposedPort(t *testing.T, world *World, endpoint *basev0.Endpoint) uint32 {
	t.Helper()
	mappings, err := world.localNetworkMappings(context.Background(),
		&resources.ServiceIdentity{Module: "saas", Name: "store"}, []*basev0.Endpoint{endpoint}, resources.NewRuntimeContextNative())
	require.NoError(t, err)
	require.Len(t, mappings, 1)
	native := resources.FilterNetworkInstance(context.Background(), mappings[0].GetInstances(), resources.NewNativeNetworkAccess())
	require.NotNil(t, native)
	return native.GetPort()
}

// Two runs in different naming scopes on one machine propose disjoint ports
// for the same service, even when their scopes derive one port.
func TestLocalRunsInTwoScopesProposeDisjointPorts(t *testing.T) {
	endpoint := claimsTestEndpoint(t)
	seen := map[uint32]string{}
	var a, b string
	for i := 0; a == "" && i < 100000; i++ {
		scope := "scope-" + strconv.Itoa(i)
		port := proposedPort(t, claimsTestWorld(t, scope, nil), endpoint)
		if other, ok := seen[port]; ok {
			a, b = other, scope
		}
		seen[port] = scope
	}
	require.NotEmpty(t, a, "no two scopes derive one port")
	// Without claims, both runs propose the one derived port: the collision.
	require.Equal(t, proposedPort(t, claimsTestWorld(t, a, nil), endpoint), proposedPort(t, claimsTestWorld(t, b, nil), endpoint))

	ledger := filepath.Join(t.TempDir(), "claims.json")
	first := proposedPort(t, claimsTestWorld(t, a, portclaims.New(ledger)), endpoint)
	second := proposedPort(t, claimsTestWorld(t, b, portclaims.New(ledger)), endpoint)
	require.NotEqual(t, first, second, "scopes %q and %q were handed the same port", a, b)

	// A restart of the first scope proposes its port again.
	require.Equal(t, first, proposedPort(t, claimsTestWorld(t, a, portclaims.New(ledger)), endpoint))
}

// A derived port another process holds is skipped at the proposal, rather
// than refused at the first Init.
func TestLocalRunSkipsAPortAnotherProcessHolds(t *testing.T) {
	endpoint := claimsTestEndpoint(t)
	derived := proposedPort(t, claimsTestWorld(t, "pc153-gate", nil), endpoint)
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(derived))))
	if err == nil {
		defer listener.Close()
	}
	require.True(t, portclaims.Held(uint16(derived)))

	world := claimsTestWorld(t, "pc153-gate", portclaims.New(filepath.Join(t.TempDir(), "claims.json")))
	port := proposedPort(t, world, endpoint)
	require.NotEqual(t, derived, port)
	// The first-Init guard that used to refuse the run passes on the claim.
	require.NoError(t, (&Runner{}).checkInitialPortAvailability(context.Background(), []*basev0.NetworkMapping{nativeMapping("tcp", uint16(port))}))
}

// A consumer deriving a producer's address before its Init, and the Init
// itself, propose the same claimed port in one run.
func TestADerivationBeforeInitAndTheInitAgree(t *testing.T) {
	endpoint := claimsTestEndpoint(t)
	ledger := portclaims.New(filepath.Join(t.TempDir(), "claims.json"))
	world := claimsTestWorld(t, "gate", ledger)
	require.Equal(t, proposedPort(t, world, endpoint), proposedPort(t, world, endpoint))
}

// A caller's --override-port wins over the claim, and is not claimed.
func TestAnOverrideWinsOverTheClaim(t *testing.T) {
	endpoint := claimsTestEndpoint(t)
	world := claimsTestWorld(t, "gate", portclaims.New(filepath.Join(t.TempDir(), "claims.json")))
	flow := &Flow{world: world}
	flow.WithPortOverrides(map[string]uint16{resources.EndpointDestination(endpoint): 45001})
	require.Equal(t, uint32(45001), proposedPort(t, world, endpoint))
}

// A derived port held by the service's own kept container — left up by a run
// of this scope before the ledger recorded it — is claimed, not moved away
// from; the same port held by anything else is.
func TestADerivedPortHeldByTheServicesOwnKeptContainerIsClaimed(t *testing.T) {
	endpoint := claimsTestEndpoint(t)
	derived := uint16(proposedPort(t, claimsTestWorld(t, "gate", nil), endpoint))
	forwarder, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(derived))))
	if err != nil {
		t.Skipf("derived port %d is not free to stand in for the container's forwarder: %v", derived, err)
	}
	defer forwarder.Close()

	identity := &resources.ServiceIdentity{Module: "saas", Name: "store"}
	kept := keptContainer(dockerrun.ContainerName(identity.UniqueWithWorkspaceAndScope("platform-core", "gate")), derived)
	kept.Labels[dockerrun.LabelCodeflyService] = "saas/store"
	previous := listOwnedContainers
	t.Cleanup(func() { listOwnedContainers = previous })
	for _, tc := range []struct {
		name    string
		service string
		moved   bool
	}{{"own container", "saas/store", false}, {"another service's container", "saas/cache", true}} {
		t.Run(tc.name, func(t *testing.T) {
			container := kept
			container.Labels = maps.Clone(kept.Labels)
			container.Labels[dockerrun.LabelCodeflyService] = tc.service
			listOwnedContainers = func(context.Context, string) ([]dockercontainer.Summary, error) {
				return []dockercontainer.Summary{container}, nil
			}
			world := claimsTestWorld(t, "gate", portclaims.New(filepath.Join(t.TempDir(), "claims.json")))
			world.containerRecoveryIdentity = testRecoveryScope + ":" + testRecoveryNamespace
			port := uint16(proposedPort(t, world, endpoint))
			require.Equal(t, tc.moved, port != derived, "derived %d, proposed %d", derived, port)
		})
	}
}
