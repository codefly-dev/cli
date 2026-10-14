package gitops

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

func TestNativeTombstoneDeliveryKeepsTheOriginalNamespace(t *testing.T) {
	module, service := nativeSolution(t, "crm")
	documents, err := (&NativeDeliverer{Workspace: &resources.Workspace{Name: "example"}, Env: identityEnvironment()}).Render(t.Context(), &NativeWorkload{
		Module: module, Service: service, Services: []*resources.Service{service},
		Subject: "system:serviceaccount:example:crm-api", SPIFFEID: "spiffe://cluster.example/ns/example/sa/crm-api",
		Artifact: NativeArtifact{Executable: filepath.Join(module.Dir(), "services", "api", "binary")}, FrontendBundle: builtBundle(),
	})
	require.NoError(t, err)
	var paths []string
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			http.Error(w, readErr.Error(), 400)
			return
		}
		paths = append(paths, r.URL.Path)
		bodies = append(bodies, body)
		_, _ = w.Write([]byte(`{"disposition":"accepted"}`))
	}))
	defer server.Close()
	var namespaces []string
	deliverer := &NativeDeliverer{Signer: &fakeSigner{}, DeliveryURL: server.URL, StateDir: t.TempDir(), Token: func(ns string) (string, error) { namespaces = append(namespaces, ns); return "token", nil }}
	_, err = deliverer.Deliver(t.Context(), documents)
	require.NoError(t, err)
	withdrawn, err := WithdrawnDocuments(documents)
	require.NoError(t, err)
	require.Empty(t, withdrawn.Presence.Workloads)
	_, err = deliverer.Deliver(t.Context(), withdrawn)
	require.NoError(t, err)
	signed, err := solutionhost.ParseSigned(bodies[len(bodies)-2])
	require.NoError(t, err)
	tombstone, err := solutionhost.Parse(signed.Document)
	require.NoError(t, err)
	require.True(t, tombstone.Removed)
	require.Empty(t, tombstone.Workloads)
	require.Equal(t, []string{"example", authorityDeliveryNamespace, "example", authorityDeliveryNamespace}, namespaces)
	require.Equal(t, []string{"/platform/_delivery/presence", "/platform/_delivery/authority", "/platform/_delivery/presence", "/platform/_delivery/authority"}, paths)
}

func TestNativeWithdrawalRetriesPartialDeliveryWithoutChangingGeneration(t *testing.T) {
	for _, fail := range []string{"presence-response", "authority"} {
		t.Run(fail, func(t *testing.T) {
			module, service := nativeSolution(t, "crm")
			documents, err := (&NativeDeliverer{Workspace: &resources.Workspace{Name: "example"}, Env: identityEnvironment()}).Render(t.Context(), &NativeWorkload{
				Module: module, Service: service, Services: []*resources.Service{service},
				Subject: "system:serviceaccount:example:crm-api", SPIFFEID: "spiffe://cluster.example/ns/example/sa/crm-api",
				Artifact: NativeArtifact{Executable: filepath.Join(module.Dir(), "services", "api", "binary")}, FrontendBundle: builtBundle(),
			})
			require.NoError(t, err)
			var presenceBodies [][]byte
			var failNext bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.URL.Path == "/platform/_delivery/presence" {
					presenceBodies = append(presenceBodies, body)
					if failNext && fail == "presence-response" {
						failNext = false
						connection, _, hijackErr := w.(http.Hijacker).Hijack()
						if hijackErr == nil {
							_ = connection.Close()
						}
						return
					}
				} else if failNext && fail == "authority" {
					failNext = false
					http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
					return
				}
				_, _ = w.Write([]byte(`{"disposition":"accepted"}`))
			}))
			defer server.Close()
			deliverer := &NativeDeliverer{Signer: &fakeSigner{}, DeliveryURL: server.URL, StateDir: t.TempDir(), Token: func(string) (string, error) { return "token", nil }}
			_, err = deliverer.Deliver(t.Context(), documents)
			require.NoError(t, err)
			withdrawn, err := WithdrawnDocuments(documents)
			require.NoError(t, err)
			failNext = true
			_, err = deliverer.Deliver(t.Context(), withdrawn)
			require.Error(t, err)
			_, err = deliverer.Deliver(t.Context(), withdrawn)
			require.NoError(t, err)
			require.Len(t, presenceBodies, 3)
			require.Equal(t, presenceBodies[1], presenceBodies[2], "ambiguous delivery must replay the exact signed tombstone")
			require.Equal(t, uint64(2), withdrawn.Presence.Generation)
			require.Equal(t, uint64(2), withdrawn.Authority.Generation)
			require.Zero(t, withdrawn.Authority.EffectiveFrom)
		})
	}
}
