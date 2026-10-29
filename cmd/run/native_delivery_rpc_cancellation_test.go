package run

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"sync/atomic"
	"testing"
	"time"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// This transport peer deliberately never answers ApplyManifest. It qualifies
// cancellation through the generated client, not authority admission semantics.
type stalledCompositionHost struct {
	compositionv1.UnimplementedCompositionServiceServer
	entered  chan struct{}
	canceled chan error
	calls    atomic.Int32
}

func (s *stalledCompositionHost) ApplyManifest(ctx context.Context, _ *compositionv1.ApplyManifestRequest) (*compositionv1.ApplyManifestResponse, error) {
	if s.calls.Add(1) == 1 {
		close(s.entered)
	}
	<-ctx.Done()
	s.canceled <- ctx.Err()
	return nil, status.FromContextError(ctx.Err()).Err()
}

func TestNativeDeliveryCancelsTheGeneratedCompositionRPC(t *testing.T) {
	for _, reason := range []string{"stop", "superseded", "deadline"} {
		t.Run(reason, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			server := grpc.NewServer()
			host := &stalledCompositionHost{entered: make(chan struct{}), canceled: make(chan error, 2)}
			compositionv1.RegisterCompositionServiceServer(server, host)
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(server.Stop)
			conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
			_, private, err := ed25519.GenerateKey(rand.Reader)
			require.NoError(t, err)
			signer, err := solutionhost.NewInMemorySigner("delivery-test", private)
			require.NoError(t, err)
			composition := gitops.NewCompositionDelivery(conn, signer, nil, nil, nil, nil, nil)
			native := &gitops.NativeDeliverer{Signer: signer, StateDir: t.TempDir(), Prepare: composition.Prepare,
				Token: func(string) (string, error) { return "test-carrier", nil }}
			manifest := &solutionhost.ModuleManifest{
				Schema: solutionhost.SchemaModuleManifestV1, Binding: "test.local.module_a", Alias: "module_a", Kind: solutionhost.KindModule,
				Host:      solutionhost.HostTarget{Coordinate: "test/local/host", Domain: "test"},
				Release:   solutionhost.Release{Publisher: "test", Name: "module_a", Version: "0.1.0"},
				Workloads: []solutionhost.Workload{{Service: "api", Container: "api"}},
				Authority: &solutionhost.Authority{Principal: "module_a"},
			}
			delivery := testDelivery(hostAlwaysReady, func(ctx context.Context) error {
				_, err := native.Deliver(ctx, manifest)
				return err
			})
			stop, superseded := make(chan struct{}), make(chan struct{})
			delivery.stop, delivery.superseded = stop, superseded
			if reason == "deadline" {
				delivery.schedule.Deadline = 200 * time.Millisecond
			}
			done := make(chan error, 1)
			go func() { done <- delivery.run(context.Background()) }()
			select {
			case <-host.entered:
			case err := <-done:
				t.Fatalf("delivery stopped before reaching ApplyManifest: %v", err)
			case <-time.After(time.Second):
				t.Fatal("ApplyManifest was not called")
			}
			want := context.DeadlineExceeded
			switch reason {
			case "stop":
				close(stop)
				want = errRunStopped
			case "superseded":
				close(superseded)
				want = errSuperseded
			}
			select {
			case err := <-done:
				require.ErrorIs(t, err, want)
				if reason == "deadline" {
					require.ErrorContains(t, err, "delivery completion was not confirmed")
					require.NotContains(t, err.Error(), "did not accept")
				}
			case <-time.After(time.Second):
				t.Fatal("generated composition RPC outlived its delivery lifecycle")
			}
			select {
			case err := <-host.canceled:
				require.Error(t, err)
			case <-time.After(time.Second):
				t.Fatal("host did not observe RPC cancellation")
			}
			require.EqualValues(t, 1, host.calls.Load(), "cancellation must not retry the RPC")
		})
	}
}
