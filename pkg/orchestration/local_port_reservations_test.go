package orchestration

import (
	"context"
	"net"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/codefly-dev/cli/pkg/portclaims"
	basev1 "github.com/codefly-dev/core/generated/go/codefly/base/v1"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func proposedMappings(t *testing.T, world *World, endpoint *basev1.Endpoint) ([]*basev1.NetworkMapping, uint16) {
	t.Helper()
	mappings, err := world.localNetworkMappings(context.Background(),
		&resources.ServiceIdentity{Module: "saas", Name: "store"}, []*basev1.Endpoint{endpoint}, resources.NewRuntimeContextNative())
	require.NoError(t, err)
	native := resources.FilterNetworkInstance(context.Background(), mappings[0].GetInstances(), resources.NewNativeNetworkAccess())
	require.NotNil(t, native)
	return mappings, uint16(native.GetPort())
}

// bindsLikeAnAgent says whether another process could be given port on the
// loopback address an agent serves its own gRPC on. An agent binds port 0 there
// and the kernel hands it any port nothing holds, so a port that can be bound
// by number is one the kernel may hand out.
func bindsLikeAnAgent(port uint16) bool {
	listener, err := net.Listen("tcp", net.JoinHostPort(loopbackIPv4, strconv.Itoa(int(port))))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

// TestAChosenPortIsHeldUntilItsServiceInitializes: a run chooses a service's
// port long before the service binds it — when the first consumer derives its
// address — and starts an agent per service in between. Each agent serves on a
// kernel-assigned loopback port, and a chosen port nothing held was the
// kernel's to hand out: on 18 Nov authority/hop's port was given to another
// service's agent of the same run, and its Init was refused "already in use".
// The run holds each chosen port until the service's first Init, whether the
// port is the derived one it claimed or one the kernel offered under
// --temporary-ports.
func TestAChosenPortIsHeldUntilItsServiceInitializes(t *testing.T) {
	for name, world := range map[string]*World{
		"a claimed derived port": claimsTestWorld(t, "pc-hold", portclaims.New(filepath.Join(t.TempDir(), "claims.json"))),
		"a temporary port": func() *World {
			world := claimsTestWorld(t, "pc-hold-temporary", nil)
			world.temporaryPorts = true
			world.LocalNetworkManager.WithTemporaryPorts()
			return world
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			endpoint := claimsTestEndpoint(t)
			mappings, port := proposedMappings(t, world, endpoint)
			t.Cleanup(world.ports.reservations.releaseAll)
			require.False(t, bindsLikeAnAgent(port), "port %d was chosen for saas/store and nothing holds it: the kernel may hand it to an agent", port)

			// A later derivation proposes the same port and still holds it.
			_, again := proposedMappings(t, world, endpoint)
			require.Equal(t, port, again)
			require.False(t, bindsLikeAnAgent(port))

			// The service's first Init takes the port over: the run lets go
			// of it, the guard sees it free, and the service binds it.
			world.releaseProposedPorts(context.Background(), mappings)
			require.NoError(t, (&Runner{}).checkInitialPortAvailability(context.Background(), mappings))
			require.True(t, bindsLikeAnAgent(port), "port %d is still held after its service's Init", port)

			// It is the service's from then on: a restart's proposal does not
			// take it back from the process that serves on it.
			_, restarted := proposedMappings(t, world, endpoint)
			require.Equal(t, port, restarted)
			require.True(t, bindsLikeAnAgent(port))
		})
	}
}

// TestAStoppedRunHoldsNoPort: a service that never reached its Init leaves no
// port held once the run stops.
func TestAStoppedRunHoldsNoPort(t *testing.T) {
	world := claimsTestWorld(t, "pc-hold-stop", portclaims.New(filepath.Join(t.TempDir(), "claims.json")))
	_, port := proposedMappings(t, world, claimsTestEndpoint(t))
	require.False(t, bindsLikeAnAgent(port))

	require.NoError(t, (&Flow{world: world}).Stop())
	require.True(t, bindsLikeAnAgent(port), "port %d is still held after the run stopped", port)

	_, after := proposedMappings(t, world, claimsTestEndpoint(t))
	require.Equal(t, port, after)
	require.True(t, bindsLikeAnAgent(port), "a stopped run held port %d again", port)
}
