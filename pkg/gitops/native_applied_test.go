package gitops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// A withdrawal is complete once the host's applied record of the binding is
// exactly it — binding, generation, digest, release and removed — read with
// the applied-state observer's credential, never the delivery's.
func TestWithdrawalCompletionRequiresExactAppliedWithdrawal(t *testing.T) {
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

func TestWithdrawalCompletionUsesTheDeclaredAppliedInterface(t *testing.T) {
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
