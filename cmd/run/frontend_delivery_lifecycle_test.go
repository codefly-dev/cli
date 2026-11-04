package run

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

func TestBatchStopsStaleRemainingSendAfterRuntimeCompletion(t *testing.T) {
	cluster, peer := admissionLauncher(t)
	service, err := cluster.composed().LoadService(t.Context(), &resources.ServiceWithModule{Module: "beta", Name: "api"})
	require.NoError(t, err)
	var once sync.Once
	peer.intercept = func(ctx context.Context, _ *compositionv1.ApplyManifestRequest, m *solutionhost.ModuleManifest) error {
		if m.Alias == "alpha" {
			once.Do(func() {
				attempt, err := cluster.beginRuntimeStart(ctx, service)
				require.NoError(t, err)
				attempt.Complete(true)
			})
		}
		return nil
	}
	require.NoError(t, testDelivery(hostAlwaysReady, cluster.admitSelected).run(t.Context()))
	for _, m := range peer.snapshots() {
		if m.Alias == "beta" {
			require.NotEmpty(t, m.Endpoints, "no pre-completion beta manifest may be sent after alpha's network wait")
		}
	}
}

func TestEditDuringRenderIsRejectedBeforeFirstSend(t *testing.T) {
	cluster, peer := admissionLauncher(t)
	file := reloadPage(t, cluster)
	var once sync.Once
	cluster.flow.(*admissionFlow).beforeCarriers = func(module string) {
		if module == "alpha" {
			once.Do(func() { require.NoError(t, os.WriteFile(file, []byte("during render"), 0600)) })
		}
	}
	require.NoError(t, hostDelivery{schedule: cluster.schedule, stop: cluster.stop, ready: hostAlwaysReady, deliver: cluster.admitSelected, log: testDelivery(hostAlwaysReady, nil).log}.run(t.Context()))
	var alpha []*solutionhost.ModuleManifest
	for _, m := range peer.snapshots() {
		if m.Alias == "alpha" {
			alpha = append(alpha, m)
		}
	}
	require.Len(t, alpha, 1, "stale rendered candidate must never reserve or send")
	require.EqualValues(t, 1, alpha[0].Generation)
}

func TestFrontendChangeDuringSendConvergesAndConcurrentAdmissionsSerialize(t *testing.T) {
	cluster, peer := admissionLauncher(t)
	file := reloadPage(t, cluster)
	require.NoError(t, cluster.admitSelected(t.Context()))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	peer.mu.Lock()
	peer.intercept = func(ctx context.Context, _ *compositionv1.ApplyManifestRequest, m *solutionhost.ModuleManifest) error {
		if m.Alias == "alpha" {
			once.Do(func() {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		}
		return ctx.Err()
	}
	peer.mu.Unlock()
	require.NoError(t, os.WriteFile(file, []byte("first edit"), 0600))
	select {
	case <-entered:
	case <-time.After(8 * time.Second):
		t.Fatal("frontend delivery did not enter host")
	}
	inFlight := latestAlpha(peer)
	require.NoError(t, os.WriteFile(file, []byte("second edit"), 0600))
	close(release)
	require.Eventually(t, func() bool {
		m := latestAlpha(peer)
		return m.Generation > inFlight.Generation && m.Frontend.Payload.Digest != inFlight.Frontend.Payload.Digest
	}, 8*time.Second, 20*time.Millisecond)
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { done <- testDelivery(hostAlwaysReady, cluster.admitSelected).run(t.Context()) }()
	}
	require.NoError(t, <-done)
	require.NoError(t, <-done)
}

func TestPartialAndFullRemovalFenceFrontendAndShutdownDrains(t *testing.T) {
	cluster, peer := admissionLauncher(t)
	file := reloadPage(t, cluster)
	workspace := composeService(t, cluster.composed(), "alpha", "worker")
	_, err := cluster.admit(t.Context(), workspace)
	require.NoError(t, err)
	flow := cluster.flow.(*admissionFlow)
	flow.selected = append(flow.selected, "alpha/worker")
	require.NoError(t, cluster.admitSelected(t.Context()))
	_, err = cluster.withdraw(t.Context(), []string{"alpha/worker"})
	require.NoError(t, err)
	cluster.mu.Lock()
	require.False(t, cluster.laneLocked("alpha").terminal)
	cluster.mu.Unlock()
	require.False(t, latestAlpha(peer).Removed)
	_, err = cluster.withdraw(t.Context(), []string{"alpha/api"})
	require.NoError(t, err)
	require.True(t, latestAlpha(peer).Removed)
	before := len(peer.snapshots())
	require.NoError(t, os.WriteFile(file, []byte("after withdrawal"), 0600))
	time.Sleep(800 * time.Millisecond)
	require.Len(t, peer.snapshots(), before)
	require.ErrorIs(t, cluster.deliver(t.Context(), "alpha"), errWorkloadGone)
	cluster.close()
	require.ErrorIs(t, cluster.admitSelected(t.Context()), errRunStopped)
	require.Len(t, peer.snapshots(), before, "no delivery after close")
}

func TestShutdownCancelsAdmittedHostCallBeforeClosingTransport(t *testing.T) {
	cluster, peer := admissionLauncher(t)
	entered := make(chan struct{})
	var once sync.Once
	peer.intercept = func(ctx context.Context, _ *compositionv1.ApplyManifestRequest, _ *solutionhost.ModuleManifest) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() { done <- cluster.admitSelected(t.Context()) }()
	<-entered
	cluster.close()
	require.Error(t, <-done)
	before := len(peer.snapshots())
	require.ErrorIs(t, cluster.deliver(t.Context(), "alpha"), errRunStopped)
	require.Len(t, peer.snapshots(), before)
}

func TestLegacyHTTPUncertainDeliveryAndAppliedReplaySameReservation(t *testing.T) {
	for _, failAt := range []string{"deliver", "applied", "withheld", "forbidden", "superseded", "removed"} {
		t.Run(failAt, func(t *testing.T) {
			cluster, peer := admissionLauncher(t)
			require.NoError(t, cluster.admitSelected(t.Context()))
			manifest := *latestAlpha(peer)
			var mu sync.Mutex
			var generations []uint64
			var gets atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					data, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					signed, err := solutionhost.ParseSigned(data)
					require.NoError(t, err)
					require.NotNil(t, signed.Envelope)
					m, err := solutionhost.ManifestFromVerified(signed.Document)
					require.NoError(t, err)
					mu.Lock()
					generations = append(generations, m.Generation)
					n := len(generations)
					mu.Unlock()
					if failAt == "deliver" && n == 1 {
						w.WriteHeader(503)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"disposition": "accepted", "generation": m.Generation})
					return
				}
				n := gets.Add(1)
				if failAt == "applied" && n == 1 {
					w.WriteHeader(503)
					return
				}
				if failAt == "forbidden" {
					w.WriteHeader(403)
					return
				}
				mu.Lock()
				generation := generations[len(generations)-1]
				mu.Unlock()
				observed := manifest
				observed.Generation = generation
				digest, err := observed.Digest()
				require.NoError(t, err)
				answer := map[string]any{"binding": manifest.Binding, "generation": generation, "digest": digest, "release": manifest.Release.Identity(), "removed": false}
				if failAt == "superseded" {
					answer["generation"] = generation + 1
				}
				if failAt == "removed" {
					answer["removed"] = true
				}
				if failAt == "withheld" {
					answer["pending"] = map[string]any{"generation": generation, "reason": "consent required"}
				}
				_ = json.NewEncoder(w).Encode(answer)
			}))
			defer server.Close()
			d := &gitops.NativeDeliverer{Signer: cluster.signer, StateDir: t.TempDir(), DeliveryURL: server.URL, Token: func(string) (string, error) { return "writer", nil }, ObserverToken: func() (string, error) { return "observer", nil }}
			err := testDelivery(hostAlwaysReady, func(ctx context.Context) error {
				if _, err := d.Deliver(ctx, &manifest); err != nil {
					return err
				}
				return d.WaitForApplied(ctx, &manifest)
			}).run(t.Context())
			mu.Lock()
			defer mu.Unlock()
			if failAt == "withheld" || failAt == "forbidden" || failAt == "superseded" || failAt == "removed" {
				require.Error(t, err)
				require.Len(t, generations, 1)
			} else {
				require.NoError(t, err)
				require.Len(t, generations, 2)
				require.Equal(t, generations[0], generations[1])
			}
		})
	}
}

func TestFrontendWatchFlagsAreExplicitAndDefaultOff(t *testing.T) {
	for _, cmd := range []string{"service", "solution", "workspace"} {
		var value string
		switch cmd {
		case "service":
			value = ServiceCmd.Flags().Lookup("watch-frontends").DefValue
		case "solution":
			value = SolutionCmd.Flags().Lookup("watch-frontends").DefValue
		case "workspace":
			value = WorkspaceCmd.Flags().Lookup("watch-frontends").DefValue
		}
		require.Equal(t, "false", value)
	}
	require.Nil(t, ClusterCmd.Flags().Lookup("watch-frontends"))
}
