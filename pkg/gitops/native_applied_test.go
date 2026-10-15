package gitops

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

func TestWithdrawalCompletionRequiresExactAppliedTombstone(t *testing.T) {
	module, service := nativeSolution(t, "crm")
	docs, err := (&NativeDeliverer{Workspace: &resources.Workspace{Name: "example"}, Env: identityEnvironment()}).Render(t.Context(), &NativeWorkload{
		Module:         module,
		Services:       []*resources.Service{service},
		FrontendBundle: builtBundle(),
		Members:        []NativeMember{{Service: service, Subject: "system:serviceaccount:example:crm-api", SPIFFEID: "spiffe://cluster.example/ns/example/sa/crm-api", Artifact: NativeArtifact{Executable: filepath.Join(module.Dir(), "services", "api", "binary")}}},
	})
	require.NoError(t, err)
	tombstones, err := WithdrawnDocuments(docs)
	require.NoError(t, err)
	p := tombstones.Presence
	digest, err := p.Digest()
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
		{name: "previous generation", status: 200, change: func(v map[string]any) { v["generation"] = p.Generation - 1; v["removed"] = false }},
		{name: "wrong binding", status: 200, change: func(v map[string]any) { v["binding"] = "other" }, wantErr: true},
		{name: "wrong host", status: 200, change: func(v map[string]any) { v["host"] = solutionhost.HostTarget{} }, wantErr: true},
		{name: "wrong digest", status: 200, change: func(v map[string]any) { v["presenceDigest"] = "other" }, wantErr: true},
		{name: "wrong release", status: 200, change: func(v map[string]any) { v["release"] = "other" }, wantErr: true},
		{name: "not removed", status: 200, change: func(v map[string]any) { v["removed"] = false }, wantErr: true},
		{name: "no applied time", status: 200, change: func(v map[string]any) { v["appliedAt"] = time.Time{} }, wantErr: true},
		{name: "later generation", status: 200, change: func(v map[string]any) { v["generation"] = p.Generation + 1 }, wantErr: true},
		{name: "observer refused", status: 403, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := map[string]any{"host": p.Host, "binding": p.Binding, "generation": p.Generation, "presenceDigest": digest, "release": p.Release.Identity(), "removed": true, "appliedAt": time.Now().UTC()}
			if tc.change != nil {
				tc.change(response)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/platform/_delivery/applied/"+p.Binding || r.Header.Get("Authorization") != "Bearer observer" {
					http.Error(w, "wrong observer request", 400)
					return
				}
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			d := &NativeDeliverer{DeliveryURL: server.URL, ObserverToken: func() (string, error) { return "observer", nil }, Token: func(string) (string, error) { t.Fatal("writer token used for observation"); return "", nil }}
			got, err := d.withdrawalApplied(t.Context(), p, digest)
			require.Equal(t, tc.want, got)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
