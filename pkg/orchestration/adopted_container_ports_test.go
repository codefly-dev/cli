package orchestration

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/require"
)

const (
	testRecoveryScope     = "1111111111111111111111111111111111111111111111111111111111111111"
	testRecoveryNamespace = "2222222222222222222222222222222222222222222222222222222222222222"
)

// keptContainer is the container a keep-running agent left up for this
// service, publishing its proposed port on 127.0.0.1 the way Core publishes it.
func keptContainer(name string, port uint16) container.Summary {
	return container.Summary{
		Names: []string{"/" + name},
		State: "running",
		Labels: map[string]string{
			dockerrun.LabelCodeflyOwner:             "true",
			dockerrun.LabelCodeflyName:              name,
			dockerrun.LabelCodeflyRecoveryScope:     testRecoveryScope,
			dockerrun.LabelCodeflyRecoveryNamespace: testRecoveryNamespace,
		},
		Ports: []container.Port{{IP: "127.0.0.1", PrivatePort: 5432, PublicPort: port, Type: "tcp"}},
	}
}

// A first Init whose proposed port is held by the service's own kept
// container — the one its agent adopts under keep-running — is not refused;
// any other holder of that port still is, on either loopback family.
func TestFirstInitAdoptsItsOwnKeptContainerAndRefusesAnyOtherHolder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*container.Summary)
		ipv6    bool // a squatter on [::1] beside the kept container
		refused bool
	}{
		{name: "kept container", refused: false},
		{name: "foreign holder on ::1 beside it", ipv6: true, refused: true},
		{name: "another recovery scope", mutate: func(c *container.Summary) { c.Labels[dockerrun.LabelCodeflyRecoveryScope] = strings.Repeat("3", 64) }, refused: true},
		{name: "another namespace", mutate: func(c *container.Summary) {
			c.Labels[dockerrun.LabelCodeflyRecoveryNamespace] = strings.Repeat("4", 64)
		}, refused: true},
		{name: "another service's name", mutate: func(c *container.Summary) {
			c.Names = []string{"/codefly-other"}
			c.Labels[dockerrun.LabelCodeflyName] = "codefly-other"
		}, refused: true},
		{name: "ephemeral", mutate: func(c *container.Summary) { c.Labels[dockerrun.LabelCodeflyEphemeral] = "true" }, refused: true},
		{name: "not running", mutate: func(c *container.Summary) { c.State = "exited" }, refused: true},
		{name: "published on another port", mutate: func(c *container.Summary) { c.Ports[0].PublicPort++ }, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Docker's port forwarder, stood in for by a listener on the address
			// the kept container publishes.
			forwarder, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer forwarder.Close()
			held := uint16(forwarder.Addr().(*net.TCPAddr).Port)
			if tc.ipv6 {
				squatter, listenErr := net.Listen("tcp", net.JoinHostPort("::1", fmt.Sprint(held)))
				if listenErr != nil {
					t.Skipf("no ::1 loopback here: %v", listenErr)
				}
				defer squatter.Close()
			}
			runner, world := gatewayRunner(t, startRealAgent(t, realAgentEcho))
			runner.containerRecoveryIdentity = testRecoveryScope + ":" + testRecoveryNamespace
			world.LocalNetworkManager.WithPortOverrides(map[string]uint16{"web/gateway/rest": held, "web/gateway/grpc": freePort(t)})
			// A run's instance identity carries no workspace: the agent is told
			// the run's workspace name at Load, and names its container with it.
			runner.instance.Identity.Workspace = ""
			kept := keptContainer(dockerrun.ContainerName(world.Workspace.Name+"-web-gateway"), held)
			if tc.mutate != nil {
				tc.mutate(&kept)
			}
			listed := ""
			previous := listOwnedContainers
			listOwnedContainers = func(_ context.Context, scopeID string) ([]container.Summary, error) {
				listed = scopeID
				return []container.Summary{kept}, nil
			}
			t.Cleanup(func() { listOwnedContainers = previous })

			_, err = runner.Init(context.Background())
			if tc.refused {
				require.ErrorContains(t, err, fmt.Sprintf("port %d (rest)", held))
				require.ErrorContains(t, err, "already in use")
				return
			}
			require.NoError(t, err)
			require.Equal(t, testRecoveryScope, listed, "only this flow's recovery scope is consulted")
		})
	}
}

// Without a recovery identity nothing can be attributed, so nothing is
// excused: the port guard refuses as before.
func TestAdoptedContainerAddressesExcuseNothingWithoutARecoveryIdentity(t *testing.T) {
	runner := &Runner{}
	require.False(t, runner.adoptedContainerAddresses(context.Background())("127.0.0.1:5432"))
}

// A publication covers only its own loopback family.
func TestPublishesAddressIsPerLoopbackFamily(t *testing.T) {
	c := container.Summary{Ports: []container.Port{{IP: "0.0.0.0", PublicPort: 5432, Type: "tcp"}}}
	require.True(t, publishesAddress(&c, "127.0.0.1:5432"))
	require.False(t, publishesAddress(&c, "[::1]:5432"))
	c.Ports = []container.Port{{IP: "::", PublicPort: 5432, Type: "tcp"}}
	require.True(t, publishesAddress(&c, "[::1]:5432"))
	require.False(t, publishesAddress(&c, "127.0.0.1:5432"))
	require.False(t, publishesAddress(&c, "localhost:5432"), "a name is never attributed")
}
