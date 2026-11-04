package gitops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Over the legacy HTTP delivery, a withdrawal is complete once the host's applied record of the binding is
// exactly it — binding, generation, digest, release and removed — read with
// the applied-state observer's credential, never the delivery's.
func TestLegacyHTTPWithdrawalCompletionRequiresExactAppliedWithdrawal(t *testing.T) {
	module, services := nativeModule(t, "crm", false)
	live := renderNative(t, module, services, "api")
	live.Generation = 3
	withdrawal, err := WithdrawalOf(live)
	require.NoError(t, err)
	digest, err := withdrawal.Digest()
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		status  int
		change  func(map[string]any)
		want    bool
		wantErr bool
	}{
		{name: "exact applied", status: 200, want: true},
		{name: "only received", status: 202, wantErr: true},
		{name: "no applied state", status: 404},
		{name: "previous generation", status: 200, change: func(v map[string]any) { v["generation"] = withdrawal.Generation - 1; v["removed"] = false }},
		{name: "wrong binding", status: 200, change: func(v map[string]any) { v["binding"] = "other" }, wantErr: true},
		{name: "wrong digest", status: 200, change: func(v map[string]any) { v["digest"] = "other" }, wantErr: true},
		{name: "wrong release", status: 200, change: func(v map[string]any) { v["release"] = "other" }, wantErr: true},
		{name: "not removed", status: 200, change: func(v map[string]any) { v["removed"] = false }, wantErr: true},
		{name: "later generation", status: 200, change: func(v map[string]any) { v["generation"] = withdrawal.Generation + 1 }, wantErr: true},
		{name: "observer refused", status: 403, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := map[string]any{"binding": withdrawal.Binding, "generation": withdrawal.Generation, "digest": digest, "release": withdrawal.Release.Identity(), "removed": true}
			if tc.change != nil {
				tc.change(response)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/platform/_delivery/applied/"+withdrawal.Binding || r.Header.Get("Authorization") != "Bearer observer" {
					http.Error(w, "wrong observer request", 400)
					return
				}
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			d := &NativeDeliverer{DeliveryURL: server.URL, ObserverToken: func() (string, error) { return "observer", nil }, Token: func(string) (string, error) { t.Fatal("writer token used for observation"); return "", nil }}
			got, err := d.withdrawalApplied(t.Context(), withdrawal, digest)
			require.Equal(t, tc.want, got)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tc.want {
				require.NoError(t, d.WaitForWithdrawal(t.Context(), withdrawal))
			}
		})
	}
	require.Error(t, (&NativeDeliverer{ObserverToken: func() (string, error) { return "", nil }}).WaitForWithdrawal(t.Context(), live), "a live manifest is no withdrawal to wait for")
}

func TestWithdrawalCompletionPrefersTheDeclaredAppliedInterface(t *testing.T) {
	module, services := nativeModule(t, "crm", false)
	live := renderNative(t, module, services, "api")
	withdrawal, err := WithdrawalOf(live)
	require.NoError(t, err)
	digest, err := withdrawal.Digest()
	require.NoError(t, err)
	calls := 0
	d := &NativeDeliverer{Applied: func(_ context.Context, binding string) (*AppliedManifest, bool, error) {
		calls++
		require.Equal(t, withdrawal.Binding, binding)
		return &AppliedManifest{Binding: binding, Generation: withdrawal.Generation, Digest: digest, Release: withdrawal.Release.Identity(), Removed: true}, true, nil
	}, ObserverToken: func() (string, error) {
		t.Error("interface completion attempted legacy observer authentication")
		return "", errors.New("legacy observer unavailable")
	}}
	require.NoError(t, d.WaitForWithdrawal(t.Context(), withdrawal))
	require.Equal(t, 1, calls)
	d.ObserverToken = nil
	require.NoError(t, d.WaitForWithdrawal(t.Context(), withdrawal))
}

// The observer describes the host's current identity, not a monotonic success
// counter for this writer. A superseding release (including removal) is terminal.
func TestLegacyHTTPAppliedRequiresTheExactDeliveredIdentity(t *testing.T) {
	module, services := nativeModule(t, "crm", false)
	manifest := renderNative(t, module, services, "api")
	manifest.Generation = 5
	digest, err := manifest.Digest()
	require.NoError(t, err)
	for _, tc := range []struct {
		name      string
		change    func(map[string]any)
		malformed bool
		previous  bool
		wantErr   string
	}{
		{name: "exact"},
		{name: "newer", change: func(v map[string]any) { v["generation"] = uint64(6) }, wantErr: "superseded"},
		{name: "newer removed", change: func(v map[string]any) { v["generation"] = uint64(6); v["removed"] = true; v["digest"] = "another" }, wantErr: "superseded"},
		{name: "wrong binding", change: func(v map[string]any) { v["binding"] = "other" }, wantErr: "binding"},
		{name: "wrong digest", change: func(v map[string]any) { v["digest"] = "another" }, wantErr: "does not match"},
		{name: "missing digest", change: func(v map[string]any) { delete(v, "digest") }, wantErr: "missing identity"},
		{name: "missing generation", change: func(v map[string]any) { delete(v, "generation") }, wantErr: "missing identity"},
		{name: "missing removed", change: func(v map[string]any) { delete(v, "removed") }, wantErr: "missing identity"},
		{name: "null removed", change: func(v map[string]any) { v["removed"] = nil }, wantErr: "missing identity"},
		{name: "wrong release", change: func(v map[string]any) { v["release"] = "another" }, wantErr: "does not match"},
		{name: "removed", change: func(v map[string]any) { v["removed"] = true }, wantErr: "does not match"},
		{name: "malformed", malformed: true, wantErr: "read applied state"},
		{name: "lower then exact", previous: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodGet, r.Method)
				require.Equal(t, "/platform/_delivery/applied/"+manifest.Binding, r.URL.Path)
				require.Equal(t, "Bearer observer", r.Header.Get("Authorization"))
				n := calls.Add(1)
				if tc.malformed {
					_, _ = w.Write([]byte(`{"generation":`))
					return
				}
				value := map[string]any{"binding": manifest.Binding, "generation": manifest.Generation, "digest": digest, "release": manifest.Release.Identity(), "removed": false}
				if tc.change != nil {
					tc.change(value)
				}
				if tc.previous && n == 1 {
					value["generation"] = manifest.Generation - 1
					value["digest"] = "prior"
					value["removed"] = true
				}
				_ = json.NewEncoder(w).Encode(value)
			}))
			defer server.Close()
			d := &NativeDeliverer{DeliveryURL: server.URL, ObserverToken: func() (string, error) { return "observer", nil }}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := d.WaitForApplied(ctx, manifest)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				var uncertain *AppliedUnconfirmed
				require.False(t, errors.As(err, &uncertain), "identity disagreement is not observation uncertainty")
			} else {
				require.NoError(t, err)
			}
			wantCalls := int32(1)
			if tc.previous {
				wantCalls = 2
			}
			require.Equal(t, wantCalls, calls.Load())
		})
	}
}
