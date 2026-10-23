package orchestration

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/run/identity"
	agentservices "github.com/codefly-dev/core/agents/services"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/workloadidentity/native"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type nativeLifecyclePeer struct {
	realAgent
	starts chan map[string]string
}

func (peer *nativeLifecyclePeer) Start(_ context.Context, request *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	peer.starts <- request.Overrides
	return &runtimev0.StartResponse{Status: &runtimev0.StartStatus{State: runtimev0.StartStatus_STARTED}}, nil
}

func TestNativeIdentityIndividualStopRestartThroughRuntimePeer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	peer := &nativeLifecyclePeer{realAgent: realAgent{mode: realAgentInputs}, starts: make(chan map[string]string, 2)}
	server := grpc.NewServer()
	runtimev0.RegisterRuntimeServer(server, peer)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	runner, world := gatewayRunner(t, &agentservices.RuntimeAgent{RuntimeClient: runtimev0.NewRuntimeClient(conn)})
	issuer, err := identity.Start("example.test", "host", t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, issuer.Close()) })
	workload := issuer.Workload("web", "gateway")
	issuer.Declare(workload)
	attester, err := native.NewAttester(issuer.Anchor())
	require.NoError(t, err)
	world.RuntimeOverrides = func(*resources.Service) (map[string]string, error) {
		paths := map[string]string{}
		for _, audience := range []string{"host", "second"} {
			credential, err := issuer.ProjectForAudience("web/gateway", workload, audience)
			if err != nil {
				return nil, err
			}
			paths[audience] = credential.File
		}
		mapping, err := json.Marshal(paths)
		return map[string]string{environments.NativeIdentityCredentials: string(mapping)}, err
	}
	world.RuntimeStopped = func(*resources.Service) error { return issuer.Forget("web/gateway") }
	previous := map[string]string{}
	for attempt := 0; attempt < 2; attempt++ {
		_, err = runner.Init(t.Context())
		require.NoError(t, err)
		_, err = runner.Start(t.Context())
		require.NoError(t, err)
		mappings := map[string]string{}
		require.NoError(t, json.Unmarshal([]byte((<-peer.starts)[environments.NativeIdentityCredentials]), &mappings))
		require.Len(t, mappings, 2)
		for audience, path := range mappings {
			token, err := os.ReadFile(path)
			require.NoError(t, err)
			_, err = attester.Attest(t.Context(), string(token), audience)
			require.NoError(t, err)
			if attempt > 0 {
				require.NotEqual(t, previous[audience], string(token))
			}
			previous[audience] = string(token)
		}
		_, err = runner.Stop(t.Context())
		require.NoError(t, err)
		require.NoError(t, issuer.Refresh())
		for _, path := range mappings {
			require.NoFileExists(t, path)
		}
	}
}
