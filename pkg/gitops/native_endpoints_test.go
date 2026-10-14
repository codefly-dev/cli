package gitops

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

// The endpoints a presence names are delivered to the host after the presence,
// signed as it was and through the same inbox; the host answers 503 until the
// generation is applied and the delivery waits for it, while a refusal that
// no wait can change is returned at once.
func TestEndpointsAreDeliveredOnceThePresenceApplies(t *testing.T) {
	ctx := context.Background()
	env := identityEnvironment()
	module, service := nativeSolution(t, "crm")
	documents, err := (&NativeDeliverer{Workspace: &resources.Workspace{Name: "example"}, Env: env}).Render(ctx, &NativeWorkload{
		Module: module, Service: service, Services: []*resources.Service{service},
		Subject: "system:serviceaccount:example:crm-api", SPIFFEID: "spiffe://cluster.example/ns/example/sa/crm-api",
		Artifact: NativeArtifact{Executable: filepath.Join(module.Dir(), "services", "api", "binary")}, FrontendBundle: builtBundle(),
	})
	require.NoError(t, err)
	presence := documents.Presence
	presence.Generation = 3
	key := resources.EndpointAsEnvironmentVariableKey(&resources.EndpointInformation{Module: "crm", Service: "api", Name: "http", API: "http"})
	raw, err := solutionhost.MarshalEndpointDelivery(presence, map[string]string{key: "http://127.0.0.1:50695"})
	require.NoError(t, err)

	var mu sync.Mutex
	var posts int
	var paths []string
	var bodies [][]byte
	var status []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		posts++
		paths = append(paths, r.URL.Path)
		bodies = append(bodies, body)
		code := http.StatusOK
		if len(status) > 0 {
			code, status = status[0], status[1:]
		}
		w.WriteHeader(code)
		switch code {
		case http.StatusServiceUnavailable:
			_, _ = w.Write([]byte(`{"error":"presence generation 3 is not applied yet"}`))
		case http.StatusOK:
			_, _ = w.Write([]byte(`{"disposition":"accepted","documentId":"example.local.crm","generation":3}`))
		default:
			_, _ = w.Write([]byte(`{"error":"stale"}`))
		}
	}))
	defer server.Close()
	deliverer := &NativeDeliverer{Signer: &fakeSigner{}, DeliveryURL: server.URL, Token: func(string) (string, error) { return "token", nil }}

	status = []int{http.StatusServiceUnavailable, http.StatusServiceUnavailable}
	receipt, err := deliverer.DeliverEndpoints(ctx, presence, raw)
	require.NoError(t, err)
	require.Equal(t, "endpoints", receipt.Kind)
	require.Equal(t, "accepted", receipt.Disposition)
	require.EqualValues(t, 3, receipt.Generation)
	require.Equal(t, 3, posts, "two refusals to wait on, then the acceptance")
	require.Equal(t, "/platform/_delivery/endpoints", paths[0])
	signed, err := solutionhost.ParseSigned(bodies[2])
	require.NoError(t, err)
	delivery, err := solutionhost.ParseEndpointDelivery(signed.Document, presence)
	require.NoError(t, err, "the body is the canonical delivery, signed, naming the presence it joins")
	require.Equal(t, "http://127.0.0.1:50695", delivery.Carriers[key])
	require.EqualValues(t, 3, delivery.Generation)

	posts = 0
	status = []int{http.StatusUnprocessableEntity}
	_, err = deliverer.DeliverEndpoints(ctx, presence, raw)
	require.ErrorContains(t, err, "refused the endpoints delivery")
	require.Equal(t, 1, posts, "a refusal no wait can change is not retried")
}
