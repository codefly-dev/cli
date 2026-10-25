package gitops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	delivery "github.com/codefly-dev/authority/libraries/delivery/go"
	runcomposition "github.com/codefly-dev/cli/pkg/composition"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

// This controlled protocol peer reuses Core admission and checks paired replay.
// It does not qualify Authority signature verification or effective permissions.
type pairedRecorder struct {
	compositionv1.UnimplementedCompositionServiceServer
	mu        sync.Mutex
	pairs     []*compositionv1.ApplyManifestRequest
	loseReply bool
	held      map[string]*compositionv1.ApplyManifestRequest
}

func (h *pairedRecorder) ApplyManifest(_ context.Context, req *compositionv1.ApplyManifestRequest) (*compositionv1.ApplyManifestResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pairs = append(h.pairs, proto.Clone(req).(*compositionv1.ApplyManifestRequest))
	manifest, err := solutionhost.ManifestFromVerified(req.Carrier.Document)
	if err != nil {
		return nil, err
	}
	var previous *solutionhost.ModuleManifest
	held := h.held[manifest.Binding]
	if held != nil {
		previous, err = solutionhost.ManifestFromVerified(held.Carrier.Document)
		if err != nil {
			return nil, err
		}
	}
	decision, err := solutionhost.Admit(manifest.Host.Coordinate, previous, manifest)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if decision == solutionhost.DecisionReplay && ((held.Authority == nil) != (req.Authority == nil) || !bytes.Equal(held.GetAuthority().GetDocument(), req.GetAuthority().GetDocument())) {
		return nil, status.Error(codes.FailedPrecondition, "policy rewrite")
	}
	if req.Authority != nil {
		var policy compositionv1.AuthorityManifestV1
		if err := proto.Unmarshal(req.Authority.Document, &policy); err != nil {
			return nil, err
		}
		if policy.Module != manifest.Alias || policy.Generation != manifest.Generation || policy.CoreManifestDigest != delivery.Digest(req.Carrier.Document) {
			return nil, status.Error(codes.InvalidArgument, "unbound authority policy")
		}
	}
	h.held[manifest.Binding] = proto.Clone(req).(*compositionv1.ApplyManifestRequest)
	if h.loseReply {
		return nil, status.Error(codes.Unavailable, "reply lost after admission")
	}
	return &compositionv1.ApplyManifestResponse{Principal: manifest.Alias, Generation: manifest.Generation}, nil
}
func (h *pairedRecorder) requests() []*compositionv1.ApplyManifestRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*compositionv1.ApplyManifestRequest(nil), h.pairs...)
}
func pairedConnection(t *testing.T) (grpc.ClientConnInterface, *pairedRecorder) {
	t.Helper()
	peer := &pairedRecorder{held: map[string]*compositionv1.ApplyManifestRequest{}}
	server := grpc.NewServer()
	compositionv1.RegisterCompositionServiceServer(server, peer)
	listener := bufconn.Listen(1 << 20)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///bufconn", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn, peer
}
func pairedManifest() *solutionhost.ModuleManifest {
	return &solutionhost.ModuleManifest{Schema: solutionhost.SchemaModuleManifestV1, Binding: "probe", Alias: "probe", Kind: solutionhost.KindSolution, Generation: 1,
		Host: solutionhost.HostTarget{Coordinate: "local", Domain: "local"}, Release: solutionhost.Release{Publisher: "test", Name: "probe", Version: "0.1.0"}, Workloads: []solutionhost.Workload{{Service: "api", Container: "app"}}, Authority: &solutionhost.Authority{Principal: "probe", ResourceKinds: []string{"probe.items"}}}
}
func TestNativeGenerationIncludesEachAuthorityPolicyInput(t *testing.T) {
	for _, input := range []string{"method", "ceiling", "role"} {
		t.Run(input, func(t *testing.T) {
			conn, peer := pairedConnection(t)
			signer := nativeSigner(t)
			action := "read"
			scopes := func() []*authorityv1.WorkScopeV1 {
				return []*authorityv1.WorkScopeV1{{ResourceKind: "probe.items", Actions: []string{action}}}
			}
			composition := NewCompositionDelivery(conn, signer,
				func(context.Context, string) ([]*compositionv1.MethodPolicyV1, bool, error) {
					if input != "method" {
						return nil, false, nil
					}
					return []*compositionv1.MethodPolicyV1{{Method: "/probe.Items/Get", Requires: scopes()}}, true, nil
				},
				func(context.Context, string) ([]*authorityv1.WorkScopeV1, error) {
					if input != "ceiling" {
						return nil, nil
					}
					return scopes(), nil
				},
				func(context.Context, string) ([]*compositionv1.RoleV1, error) {
					if input != "role" {
						return nil, nil
					}
					return []*compositionv1.RoleV1{{Name: "viewer", Scopes: scopes()}}, nil
				})
			d := &NativeDeliverer{Signer: signer, StateDir: t.TempDir(), Prepare: composition.Prepare, Token: func(string) (string, error) { return "credential", nil }}
			_, err := d.Deliver(t.Context(), pairedManifest())
			require.NoError(t, err)
			_, err = d.Deliver(t.Context(), pairedManifest())
			require.NoError(t, err)
			action = "write"
			_, err = d.Deliver(t.Context(), pairedManifest())
			require.NoError(t, err)
			_, err = d.Deliver(t.Context(), pairedManifest())
			require.NoError(t, err)
			var generations []uint64
			for _, request := range peer.requests() {
				manifest, err := solutionhost.ManifestFromVerified(request.Carrier.Document)
				require.NoError(t, err)
				generations = append(generations, manifest.Generation)
			}
			require.Equal(t, []uint64{1, 1, 2, 2}, generations, "policy-only edit must advance once; unchanged pair must replay")
		})
	}
}

func TestNativeLostAcknowledgementCannotReuseGenerationForDifferentPolicy(t *testing.T) {
	conn, peer := pairedConnection(t)
	signer := nativeSigner(t)
	action := "read"
	composition := NewCompositionDelivery(conn, signer, func(context.Context, string) ([]*compositionv1.MethodPolicyV1, bool, error) {
		return []*compositionv1.MethodPolicyV1{{Method: "/probe.Items/Get", Requires: []*authorityv1.WorkScopeV1{{ResourceKind: "probe.items", Actions: []string{action}}}}}, true, nil
	}, nil, nil)
	d := &NativeDeliverer{Signer: signer, StateDir: t.TempDir(), Prepare: composition.Prepare, Token: func(string) (string, error) { return "credential", nil }}
	peer.mu.Lock()
	peer.loseReply = true
	peer.mu.Unlock()
	_, err := d.Deliver(t.Context(), pairedManifest())
	require.Error(t, err)
	d = &NativeDeliverer{Signer: signer, StateDir: d.StateDir, Prepare: composition.Prepare, Token: d.Token}
	peer.mu.Lock()
	peer.loseReply = false
	peer.mu.Unlock()
	_, err = d.Deliver(t.Context(), pairedManifest())
	require.NoError(t, err)
	action = "write"
	_, err = d.Deliver(t.Context(), pairedManifest())
	require.NoError(t, err)
	pairs := peer.requests()
	require.Equal(t, pairs[0].Carrier.Document, pairs[1].Carrier.Document)
	require.Equal(t, pairs[0].Authority.Document, pairs[1].Authority.Document)
	first, err := solutionhost.ManifestFromVerified(pairs[0].Carrier.Document)
	require.NoError(t, err)
	second, err := solutionhost.ManifestFromVerified(pairs[2].Carrier.Document)
	require.NoError(t, err)
	require.Greater(t, second.Generation, first.Generation, "uncertain first reply must not permit rewriting its attempted generation")
}

func TestNativePolicySnapshotCannotChangeBetweenSettlementAndSend(t *testing.T) {
	conn, peer := pairedConnection(t)
	signer := nativeSigner(t)
	policy := &compositionv1.MethodPolicyV1{Method: "/probe.Items/Get", Requires: []*authorityv1.WorkScopeV1{{ResourceKind: "probe.items", Actions: []string{"read"}}}}
	reads := 0
	composition := NewCompositionDelivery(conn, signer, func(context.Context, string) ([]*compositionv1.MethodPolicyV1, bool, error) {
		reads++
		return []*compositionv1.MethodPolicyV1{policy}, true, nil
	}, nil, nil)
	d := &NativeDeliverer{Signer: signer, StateDir: t.TempDir(), Prepare: composition.Prepare, Token: func(string) (string, error) { policy.Requires[0].Actions[0] = "write"; return "credential", nil }}
	_, err := d.Deliver(t.Context(), pairedManifest())
	require.NoError(t, err)
	var sent compositionv1.AuthorityManifestV1
	require.NoError(t, proto.Unmarshal(peer.requests()[0].Authority.Document, &sent))
	require.Equal(t, []string{"read"}, sent.Methods[0].Requires[0].Actions, "send must use the private snapshot settled before credential acquisition")
	require.Equal(t, 1, reads, "one source read for one delivery attempt")
}

func TestPairedPolicyPresenceAndWithdrawalReplay(t *testing.T) {
	conn, peer := pairedConnection(t)
	signer := nativeSigner(t)
	declared, reads := false, 0
	c := NewCompositionDelivery(conn, signer, func(context.Context, string) ([]*compositionv1.MethodPolicyV1, bool, error) {
		reads++
		return nil, declared, nil
	}, nil, nil)
	d := &NativeDeliverer{Signer: signer, StateDir: t.TempDir(), Prepare: c.Prepare, Token: func(string) (string, error) { return "credential", nil }}
	for _, present := range []bool{false, false, true, true, false} {
		declared = present
		_, err := d.Deliver(t.Context(), pairedManifest())
		require.NoError(t, err)
	}
	var generations []uint64
	for _, pair := range peer.requests() {
		m, err := solutionhost.ManifestFromVerified(pair.Carrier.Document)
		require.NoError(t, err)
		generations = append(generations, m.Generation)
	}
	require.Equal(t, []uint64{1, 1, 2, 2, 3}, generations)
	require.NotNil(t, peer.requests()[2].Authority, "present empty policy is distinct from absence")
	withdrawal, err := WithdrawalOf(pairedManifest())
	require.NoError(t, err)
	for range 2 {
		_, err = d.Deliver(t.Context(), withdrawal)
		require.NoError(t, err)
	}
	require.Equal(t, uint64(4), withdrawal.Generation)
	require.Equal(t, 5, reads, "withdrawal must not read removed sources")
	require.Nil(t, peer.requests()[5].Authority)
}

func TestPairedMigrationPreservesTerminalGeneration(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(fmt.Sprint(removed), func(t *testing.T) {
			conn, peer := pairedConnection(t)
			signer := nativeSigner(t)
			c := NewCompositionDelivery(conn, signer, nil, nil, nil)
			d := &NativeDeliverer{Signer: signer, StateDir: t.TempDir(), Prepare: c.Prepare, Token: func(string) (string, error) { return "credential", nil }}
			manifest := pairedManifest()
			if removed {
				var err error
				manifest, err = WithdrawalOf(manifest)
				require.NoError(t, err)
			}
			manifest.Generation = 1
			content, err := solutionhost.SignedPayload(manifest)
			require.NoError(t, err)
			record := &nativeGenerations{Entries: map[string]nativeGeneration{}}
			settle(record, manifest.Binding, content, false)
			entry := record.Entries[manifest.Binding]
			entry.Generation = 7
			record.Entries[manifest.Binding] = entry
			require.NoError(t, d.saveGenerations(t.Context(), record))
			manifest.Generation = 7
			document, err := solutionhost.SignedPayload(manifest)
			require.NoError(t, err)
			peer.held[manifest.Binding] = &compositionv1.ApplyManifestRequest{Carrier: &compositionv1.SignedManifestV1{Document: document}}
			for range 2 {
				_, err = d.Deliver(t.Context(), manifest)
				require.NoError(t, err)
			}
			want := uint64(8)
			if removed {
				want = 7
			}
			require.Equal(t, want, manifest.Generation, "legacy live advances once; legacy tombstone replays exactly")
		})
	}
}

func TestNativeReservationSerializesConcurrentDeliverers(t *testing.T) {
	for _, sameBinding := range []bool{false, true} {
		t.Run(fmt.Sprint(sameBinding), func(t *testing.T) {
			dir := t.TempDir()
			signer := nativeSigner(t)
			ready := make(chan uint64, 2)
			release := make(chan struct{})
			defer close(release)
			done := make(chan error, 2)
			for i := range 2 {
				go func() {
					post := func(_ context.Context, _ string, signed *solutionhost.Signed, _ string) (Receipt, error) {
						m, err := solutionhost.ManifestFromVerified(signed.Document)
						if err != nil {
							return Receipt{}, err
						}
						ready <- m.Generation
						<-release
						return Receipt{}, nil
					}
					d := &NativeDeliverer{Signer: signer, StateDir: dir, Token: func(string) (string, error) { return "credential", nil },
						Prepare: func(context.Context, string, bool) (*PreparedDelivery, error) {
							return &PreparedDelivery{Content: []byte{0}, Post: post}, nil
						}}
					m := pairedManifest()
					m.Release.Version = fmt.Sprintf("0.1.%d", i)
					if !sameBinding {
						m.Binding = fmt.Sprintf("probe%d", i)
					}
					_, err := d.Deliver(t.Context(), m)
					done <- err
				}()
			}
			// Both sends can arrive before either returns: no RPC holds the file lock.
			got := []uint64{}
			for range 2 {
				select {
				case n := <-ready:
					got = append(got, n)
				case <-time.After(3 * time.Second):
					t.Fatal("reservation lost or network held the shared lock")
				}
			}
			record, err := (&NativeDeliverer{StateDir: dir}).loadGenerations()
			require.NoError(t, err)
			if sameBinding {
				require.ElementsMatch(t, []uint64{1, 2}, got)
				require.Len(t, record.Entries, 1)
			} else {
				require.Equal(t, []uint64{1, 1}, got)
				require.Len(t, record.Entries, 2)
			}
			release <- struct{}{}
			release <- struct{}{}
			for range 2 {
				require.NoError(t, <-done)
			}
		})
	}
}

func TestNativeCanceledLockWaitDoesNotReadReserveOrSend(t *testing.T) {
	d := &NativeDeliverer{StateDir: t.TempDir(), Prepare: func(context.Context, string, bool) (*PreparedDelivery, error) {
		t.Error("prepared after cancellation")
		return nil, errors.New("unexpected")
	}}
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- runcomposition.WithFileLock(d.generationsPath()+".lock", time.Second, func() error { close(entered); <-release; return nil })
	}()
	<-entered
	defer func() { close(release); require.NoError(t, <-finished) }()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := d.Deliver(ctx, pairedManifest())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoFileExists(t, d.generationsPath())
}

func TestNativeReservationFailureNeverSends(t *testing.T) {
	d := &NativeDeliverer{StateDir: t.TempDir()}
	// Sabotage the write after the record has been loaded; no fake atomic writer.
	d.Prepare = func(context.Context, string, bool) (*PreparedDelivery, error) {
		require.NoError(t, os.Mkdir(d.generationsPath(), 0700))
		return &PreparedDelivery{Post: func(context.Context, string, *solutionhost.Signed, string) (Receipt, error) {
			t.Error("sent without reservation")
			return Receipt{}, nil
		}}, nil
	}
	_, err := d.Deliver(t.Context(), pairedManifest())
	require.Error(t, err)
}

func TestNativePreparationFailureDoesNotReserve(t *testing.T) {
	d := &NativeDeliverer{StateDir: t.TempDir(), Prepare: func(context.Context, string, bool) (*PreparedDelivery, error) {
		return nil, errors.New("policy source unavailable")
	}}
	_, err := d.Deliver(t.Context(), pairedManifest())
	require.ErrorContains(t, err, "policy source unavailable")
	require.NoFileExists(t, d.generationsPath())
}

func TestNativeFailedSigningRetainsReservation(t *testing.T) {
	d := &NativeDeliverer{StateDir: t.TempDir(), Prepare: func(context.Context, string, bool) (*PreparedDelivery, error) {
		return &PreparedDelivery{Content: []byte{0}, Post: func(context.Context, string, *solutionhost.Signed, string) (Receipt, error) {
			return Receipt{}, errors.New("sent without a signature")
		}}, nil
	}}
	_, err := d.Deliver(t.Context(), pairedManifest())
	require.Error(t, err)
	data, err := os.ReadFile(filepath.Join(d.StateDir, "generations.json"))
	require.NoError(t, err)
	var record nativeGenerations
	require.NoError(t, json.Unmarshal(data, &record))
	require.Equal(t, uint64(1), record.Entries["probe"].Generation)
}

// A module re-added after its withdrawal is delivered at the next generation
// whatever happened to the withdrawal: the local record never refuses, and
// whether the withdrawal is terminal is the host's to say. A host that still
// holds it refuses by naming what to do; a host that never received it (the
// signing failed) or that started again with nothing held accepts the module.
func TestNativeRevivalAfterAWithdrawalIsTheHostsToRefuse(t *testing.T) {
	for _, outcome := range []string{"acknowledged", "lost-response", "failed-sign"} {
		t.Run(outcome, func(t *testing.T) {
			conn, peer := pairedConnection(t)
			signer := nativeSigner(t)
			c := NewCompositionDelivery(conn, signer, nil, nil, nil)
			d := &NativeDeliverer{Signer: signer, StateDir: t.TempDir(), Prepare: c.Prepare, Token: func(string) (string, error) { return "credential", nil }}
			_, err := d.Deliver(t.Context(), pairedManifest())
			require.NoError(t, err)
			withdrawal, err := WithdrawalOf(pairedManifest())
			require.NoError(t, err)
			if outcome == "lost-response" {
				peer.mu.Lock()
				peer.loseReply = true
				peer.mu.Unlock()
			}
			if outcome == "failed-sign" {
				d.Signer = nil
			}
			_, err = d.Deliver(t.Context(), withdrawal)
			if outcome == "acknowledged" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Equal(t, uint64(2), withdrawal.Generation)
			peer.mu.Lock()
			peer.loseReply = false
			peer.mu.Unlock()

			d = &NativeDeliverer{Signer: signer, StateDir: d.StateDir, Prepare: c.Prepare, Token: d.Token}
			revived := pairedManifest()
			_, err = d.Deliver(t.Context(), revived)
			require.Equal(t, uint64(3), revived.Generation, "a delivery after a withdrawal is the next generation")
			if outcome == "failed-sign" {
				require.NoError(t, err, "a host that never received the withdrawal accepts the module")
				return
			}
			require.ErrorIs(t, err, solutionhost.ErrWithdrawn)
			require.ErrorContains(t, err, "stop the run and start it again")
			var refusal *HostStatusError
			require.ErrorAs(t, err, &refusal)
			require.False(t, refusal.Transient())

			restarted, _ := pairedConnection(t)
			fresh := NewCompositionDelivery(restarted, signer, nil, nil, nil)
			d = &NativeDeliverer{Signer: signer, StateDir: d.StateDir, Prepare: fresh.Prepare, Token: d.Token}
			_, err = d.Deliver(t.Context(), pairedManifest())
			require.NoError(t, err, "a host started again accepts the module at the reserved generation")
		})
	}
}
