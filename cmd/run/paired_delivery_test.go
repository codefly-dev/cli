package run

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/run/identity"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
)

type pairedLauncherFlow struct {
	fakeLauncherFlow
	address string
}

func (f *pairedLauncherFlow) GRPCEndpointTarget(context.Context, string, string, string) (orchestration.GRPCTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return orchestration.GRPCTarget{Address: f.address}, nil
}

// The launcher witness checks actual factory wiring and filesystem readers.
// Authority admission itself is exercised in its owning repository.
type launcherPolicyPeer struct {
	compositionv1.UnimplementedCompositionServiceServer
	mu          sync.Mutex
	generations []uint64
}

func (p *launcherPolicyPeer) ApplyManifest(_ context.Context, r *compositionv1.ApplyManifestRequest) (*compositionv1.ApplyManifestResponse, error) {
	m, err := solutionhost.ManifestFromVerified(r.GetCarrier().GetDocument())
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.generations = append(p.generations, m.Generation)
	p.mu.Unlock()
	return &compositionv1.ApplyManifestResponse{Generation: m.Generation}, nil
}

func TestLauncherReservesGenerationFromChangedSolutionRoles(t *testing.T) {
	workspace, env := writeHostedNativeWorkspace(t)
	moduleFile := filepath.Join(workspace.Dir(), "modules", "model", resources.ModuleConfigurationName)
	require.NoError(t, os.WriteFile(moduleFile, []byte("kind: module\nname: model\nservices:\n  - name: model\ninterface:\n  endpoints:\n    - service: model\n      endpoint: rest\n      implements: [codefly.dev/composition@1.0.0]\n"), 0600))
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), workspace.Dir())
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	peer := &launcherPolicyPeer{}
	server := grpc.NewServer()
	compositionv1.RegisterCompositionServiceServer(server, peer)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := solutionhost.NewInMemorySigner("test", key)
	require.NoError(t, err)
	vouch, err := identity.Start(env.Host.TrustDomain, env.Host.Audience, t.TempDir())
	require.NoError(t, err)
	flow := &pairedLauncherFlow{address: listener.Addr().String()}
	cluster := &launcher{workspace: workspace, env: env, signer: signer, vouch: vouch, stateDir: t.TempDir(), hostModule: "model", hostService: "model", hostEndpoint: "rest", flow: flow, stop: make(chan struct{})}
	cluster.declare(map[string]gitops.NativeIdentity{"model/model": {Module: "model", Service: "model"}})
	contractPath := filepath.Join(workspace.Dir(), "modules", "model", modulecontract.FileName)
	var conns []*grpc.ClientConn
	for _, action := range []string{"read", "read", "write", "write"} {
		contract := fmt.Sprintf("schema: codefly/module-contract/v1\nprincipal: model\nnamespaces: []\nqueues: []\nscope_ceilings:\n  - resource_kind: model.item\n    actions: [read, write]\nroles:\n  - name: installer\n    scopes:\n      - resource_kind: model.item\n        actions: [%s]\nbindings: []\ndestinations: []\n", action)
		require.NoError(t, os.WriteFile(contractPath, []byte(contract), 0600))
		// The production retry path constructs a new deliverer each attempt,
		// over the run's one connection to the host.
		d, gateway, err := cluster.newDeliverer(t.Context())
		require.NoError(t, err)
		require.Empty(t, gateway, "a composition host takes no envelope")
		require.NotNil(t, d.Prepare)
		if conns = append(conns, cluster.host.conn); len(conns) > 1 {
			require.Same(t, conns[0], cluster.host.conn, "every attempt reuses the run's connection to the host")
		}
		m := &solutionhost.ModuleManifest{Schema: solutionhost.SchemaModuleManifestV1, Binding: "probe", Alias: "model", Kind: solutionhost.KindSolution,
			Host: solutionhost.HostTarget{Coordinate: "local", Domain: "local"}, Release: solutionhost.Release{Publisher: "test", Name: "model", Version: "0.1.0"},
			Workloads: []solutionhost.Workload{{Service: "model", Container: "app"}}, Authority: &solutionhost.Authority{Principal: "model", ResourceKinds: []string{"model.item"}}}
		_, err = d.Deliver(t.Context(), m)
		require.NoError(t, err)
	}
	peer.mu.Lock()
	require.Equal(t, []uint64{1, 1, 2, 2}, peer.generations)
	peer.mu.Unlock()

	// A host answering at another address is dialled again, and the
	// connection it replaces is closed; the run's teardown closes the last.
	moved, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	movedServer := grpc.NewServer()
	compositionv1.RegisterCompositionServiceServer(movedServer, peer)
	go func() { _ = movedServer.Serve(moved) }()
	t.Cleanup(movedServer.Stop)
	flow.mu.Lock()
	flow.address = moved.Addr().String()
	flow.mu.Unlock()
	_, _, err = cluster.newDeliverer(t.Context())
	require.NoError(t, err)
	require.NotSame(t, conns[0], cluster.host.conn)
	require.Equal(t, connectivity.Shutdown, conns[0].GetState())
	last := cluster.host.conn
	cluster.close()
	require.Equal(t, connectivity.Shutdown, last.GetState())
	_, _, err = cluster.newDeliverer(t.Context())
	require.ErrorIs(t, err, errRunStopped, "a stopped run dials nothing")
}
