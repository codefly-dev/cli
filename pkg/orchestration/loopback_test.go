package orchestration

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// listenOn opens a TCP listener on host (loopback only, or every interface
// for "") and returns its mappings as a native run records them: one http
// endpoint, its native instance at the listener's port.
func listenOn(t *testing.T, host string) ([]*basev0.NetworkMapping, uint16) {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	instance := resources.NewHTTPNetworkInstance("localhost", port, false)
	instance.Access = resources.NewNativeNetworkAccess()
	return []*basev0.NetworkMapping{{
		Endpoint:  &basev0.Endpoint{Name: "http", Api: "http"},
		Instances: []*basev0.NetworkInstance{instance},
	}}, port
}

// A service that listens on loopback answers on no address a sibling machine
// could dial; one that binds every interface answers on each of this
// machine's, and that is what the run warns about.
func TestOnlyAServiceBoundOffLoopbackAnswersThere(t *testing.T) {
	addresses := machineAddresses()
	if len(addresses) == 0 {
		t.Skip("this machine has no non-loopback address to probe")
	}
	ctx := context.Background()
	loopback, _ := listenOn(t, "127.0.0.1")
	require.Empty(t, offLoopbackAnswers(ctx, loopback, addresses, time.Second), "a loopback listener is reachable from nowhere else")

	every, port := listenOn(t, "")
	answers := offLoopbackAnswers(ctx, every, addresses, time.Second)
	require.NotEmpty(t, answers, "a listener on every interface answers on the machine's addresses")
	for _, answer := range answers {
		host, answeredPort, err := net.SplitHostPort(answer)
		require.NoError(t, err)
		require.Equal(t, strconv.Itoa(int(port)), answeredPort)
		require.False(t, net.ParseIP(host).IsLoopback())
	}

	t.Run("a closed port answers nowhere", func(t *testing.T) {
		closed, _ := listenOn(t, "")
		instance := closed[0].Instances[0]
		instance.Port = instance.Port + 1 // nothing listens there
		require.Empty(t, offLoopbackAnswers(ctx, closed, addresses, 200*time.Millisecond))
	})
	t.Run("no machine address, no probe", func(t *testing.T) {
		require.Nil(t, offLoopbackAnswers(ctx, every, nil, time.Second))
	})
}
