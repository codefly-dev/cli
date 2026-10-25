package gitops

// The legacy HTTP delivery: a host whose delivery endpoint implements no
// codefly.dev/composition (the saas host on main) receives the carrier on its
// manifest route and the envelope through its gateway.

import (
	"context"
	"encoding/json"
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

// httpDeliveryHost stands in for the host's delivery listener: it records every
// carrier, the credential it came with, and answers with the status queued.
type httpDeliveryHost struct {
	*httptest.Server
	mu        sync.Mutex
	paths     []string
	bearers   []string
	manifests []*solutionhost.ModuleManifest
	bodies    [][]byte
	status    []int
}

func newHTTPDeliveryHost(t *testing.T) *httpDeliveryHost {
	t.Helper()
	host := &httpDeliveryHost{}
	host.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		signed, err := solutionhost.ParseSigned(body)
		if err != nil {
			http.Error(w, `{"error":"not a carrier"}`, http.StatusBadRequest)
			return
		}
		manifest, err := solutionhost.ManifestFromVerified(signed.Document)
		if err != nil {
			http.Error(w, `{"error":"not a manifest"}`, http.StatusBadRequest)
			return
		}
		host.mu.Lock()
		defer host.mu.Unlock()
		host.paths = append(host.paths, r.URL.Path)
		host.bearers = append(host.bearers, r.Header.Get("Authorization"))
		host.manifests = append(host.manifests, manifest)
		host.bodies = append(host.bodies, body)
		code := http.StatusOK
		if len(host.status) > 0 {
			code, host.status = host.status[0], host.status[1:]
		}
		w.WriteHeader(code)
		if code == http.StatusOK {
			_ = json.NewEncoder(w).Encode(map[string]any{"disposition": "accepted", "generation": manifest.Generation})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "module manifest generation is older than the applied generation"})
	}))
	t.Cleanup(host.Close)
	return host
}

// deliverer is a native deliverer to the host, its carrier credentials issued
// per module and recorded.
func (h *httpDeliveryHost) deliverer(t *testing.T, state string, modules *[]string) *NativeDeliverer {
	return &NativeDeliverer{
		Workspace: &resources.Workspace{Name: "example"}, Env: nativeEnvironment(), Signer: nativeSigner(t),
		DeliveryURL: h.URL, StateDir: state,
		Token: func(module string) (string, error) {
			*modules = append(*modules, module)
			return "delivery-" + module, nil
		},
	}
}

// TestANativeGenerationMovesOnlyWhenTheContentDoes: one carrier per delivery,
// to the manifest route, with the delivery credential of the workloads'
// namespace. The same content replays its generation, byte for byte; changed
// content — a second workload started — is the next one; and the record
// outlives the deliverer, so a restarted run replays rather than restarting
// at 1.
func TestTheLegacyHTTPDeliveryMovesTheGenerationOnlyWhenTheContentDoes(t *testing.T) {
	module, services := nativeModule(t, "crm", false)
	host := newHTTPDeliveryHost(t)
	state := t.TempDir()
	var modules []string
	deliverer := host.deliverer(t, state, &modules)

	receipt, err := deliverer.Deliver(t.Context(), renderNative(t, module, services, "api"))
	require.NoError(t, err)
	require.Equal(t, Receipt{Status: http.StatusOK, Disposition: "accepted", Generation: 1}, receipt)
	_, err = deliverer.Deliver(t.Context(), renderNative(t, module, services, "api"))
	require.NoError(t, err)
	both := renderNative(t, module, services, "api", "worker")
	_, err = deliverer.Deliver(t.Context(), both)
	require.NoError(t, err)
	require.Equal(t, uint64(2), both.Generation, "Deliver stamps the settled generation on the manifest")

	restarted := host.deliverer(t, state, &modules)
	_, err = restarted.Deliver(t.Context(), renderNative(t, module, services, "api", "worker"))
	require.NoError(t, err)

	require.Equal(t, []string{manifestDeliveryPath, manifestDeliveryPath, manifestDeliveryPath, manifestDeliveryPath}, host.paths)
	require.Equal(t, []string{"crm", "crm", "crm", "crm"}, modules)
	require.Equal(t, "Bearer delivery-crm", host.bearers[0])
	generations := []uint64{}
	for _, manifest := range host.manifests {
		generations = append(generations, manifest.Generation)
	}
	require.Equal(t, []uint64{1, 1, 2, 2}, generations)
	require.Equal(t, string(host.bodies[0]), string(host.bodies[1]), "unchanged content is the same signed carrier")
	require.FileExists(t, filepath.Join(state, "generations.json"))
}

// A host's refusal (409: a stale or rewritten generation, a withdrawn
// binding) is an error with its reason, and retains the reservation: delivering the
// same content again settles to the same generation.
func TestALegacyHTTPRefusalRetainsItsReservation(t *testing.T) {
	module, services := nativeModule(t, "crm", false)
	host := newHTTPDeliveryHost(t)
	host.status = []int{http.StatusConflict}
	var modules []string
	deliverer := host.deliverer(t, t.TempDir(), &modules)
	receipt, err := deliverer.Deliver(t.Context(), renderNative(t, module, services, "api"))
	require.ErrorContains(t, err, "older than the applied generation")
	require.Equal(t, http.StatusConflict, receipt.Status)
	_, err = deliverer.Deliver(t.Context(), renderNative(t, module, services, "api"))
	require.NoError(t, err)
	require.Equal(t, uint64(1), host.manifests[1].Generation, "the refused delivery retained its reserved generation")
}

// TestANativeWithdrawalIsARemovedManifestAtTheNextGeneration: what withdraws
// a module a native run delivered is its manifest's identity alone, removed,
// at the next generation — the same withdrawal a publication settles —
// delivered with the credential of the module's namespace, and replayed as it
// was until the host has applied it.
func TestALegacyHTTPWithdrawalIsARemovedManifestAtTheNextGeneration(t *testing.T) {
	module, services := nativeModule(t, "crm", false)
	host := newHTTPDeliveryHost(t)
	var modules []string
	deliverer := host.deliverer(t, t.TempDir(), &modules)
	live := renderNative(t, module, services, "api", "worker")
	_, err := deliverer.Deliver(t.Context(), live)
	require.NoError(t, err)

	withdrawal, err := WithdrawalOf(live)
	require.NoError(t, err)
	_, err = deliverer.Deliver(t.Context(), withdrawal)
	require.NoError(t, err)
	replay, err := WithdrawalOf(live)
	require.NoError(t, err)
	_, err = deliverer.Deliver(t.Context(), replay)
	require.NoError(t, err)

	posted := host.manifests[1]
	require.True(t, posted.Removed)
	require.Equal(t, uint64(2), posted.Generation)
	require.Equal(t, live.Binding, posted.Binding)
	require.Empty(t, posted.Workloads)
	require.Empty(t, posted.Endpoints)
	require.Nil(t, posted.Authority)
	require.Equal(t, []string{"crm", "crm", "crm"}, modules, "a withdrawal is delivered by the module's carrier")
	require.Equal(t, string(host.bodies[1]), string(host.bodies[2]), "a withdrawal is replayed byte for byte")
	require.False(t, live.Removed, "the delivered manifest is untouched")
}

// envelopeHost stands in for the host's administrator API: fixture sign-in,
// the envelope in force, and conditional writes.
type envelopeHost struct {
	*httptest.Server
	mu       sync.Mutex
	revision uint64
	document string
	writes   []map[string]any
	signIns  int
	refusal  int
}

func newEnvelopeHost(t *testing.T) *envelopeHost {
	t.Helper()
	host := &envelopeHost{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/authenticate", func(w http.ResponseWriter, r *http.Request) {
		host.mu.Lock()
		host.signIns++
		host.mu.Unlock()
		var body struct {
			Provider string            `json:"provider"`
			Fixture  map[string]string `json:"fixture"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Provider != "email" || body.Fixture["token"] != "dev-admin" {
			http.Error(w, "unknown fixture", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "session"})
	})
	authorised := func(w http.ResponseWriter, r *http.Request) bool {
		host.mu.Lock()
		refusal := host.refusal
		host.mu.Unlock()
		if refusal != 0 {
			http.Error(w, "refused", refusal)
			return false
		}
		if r.Header.Get("Authorization") != "Bearer session" {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /v1/platform/module-envelope", func(w http.ResponseWriter, r *http.Request) {
		if !authorised(w, r) {
			return
		}
		host.mu.Lock()
		defer host.mu.Unlock()
		if host.revision == 0 {
			http.Error(w, "none", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"envelope": map[string]any{"revision": host.revision, "document": host.document}})
	})
	mux.HandleFunc("PUT /v1/platform/module-envelope", func(w http.ResponseWriter, r *http.Request) {
		if !authorised(w, r) {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		host.mu.Lock()
		defer host.mu.Unlock()
		if uint64(body["expectedRevision"].(float64)) != host.revision {
			http.Error(w, "conflict", http.StatusConflict)
			return
		}
		host.writes = append(host.writes, body)
		host.revision++
		host.document = body["document"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	})
	host.Server = httptest.NewServer(mux)
	t.Cleanup(host.Close)
	return host
}

func (h *envelopeHost) writer() *EnvelopeWriter {
	return &EnvelopeWriter{GatewayURL: h.URL, FixtureProvider: "email", FixtureToken: "dev-admin"}
}

func grantsOf(principals ...string) map[string]EnvelopeGrant {
	grants := map[string]EnvelopeGrant{}
	for _, principal := range principals {
		grants[principal] = EnvelopeGrant(`{"queues":["` + principal + `.jobs"],"cross_tenant":false,"tenant":""}`)
	}
	return grants
}

// The envelope a run writes is grants only: each module principal's grant, as
// the composition states it, under the revision — no approved builds, no
// workload identities, no bindings.
func TestTheEnvelopeIsEachPrincipalsGrant(t *testing.T) {
	document, err := BuildEnvelope(3, grantsOf("wiki", "proposals"))
	require.NoError(t, err)
	require.JSONEq(t, `{"revision":3,"principals":{
		"wiki":{"grant":{"queues":["wiki.jobs"],"cross_tenant":false,"tenant":""}},
		"proposals":{"grant":{"queues":["proposals.jobs"],"cross_tenant":false,"tenant":""}}}}`, string(document))
}

// Ensure writes the next revision, expecting the one it read, only when the
// envelope in force does not already say what the run would write.
func TestEnsureWritesTheEnvelopeOnlyWhenItChanges(t *testing.T) {
	host := newEnvelopeHost(t)
	writer := host.writer()
	build := func(principals ...string) func(uint64) ([]byte, error) {
		return func(revision uint64) ([]byte, error) { return BuildEnvelope(revision, grantsOf(principals...)) }
	}
	revision, err := writer.Ensure(context.Background(), build("wiki"))
	require.NoError(t, err)
	require.Equal(t, uint64(1), revision, "a host with no envelope is written its first")
	require.Len(t, host.writes, 1)
	require.Equal(t, float64(0), host.writes[0]["expectedRevision"])

	revision, err = writer.Ensure(context.Background(), build("wiki"))
	require.NoError(t, err)
	require.Equal(t, uint64(1), revision, "the same content is the revision in force")
	require.Len(t, host.writes, 1, "an unchanged envelope is not rewritten")

	revision, err = writer.Ensure(context.Background(), build("wiki", "proposals"))
	require.NoError(t, err)
	require.Equal(t, uint64(2), revision)
	require.Len(t, host.writes, 2)
	require.Equal(t, float64(1), host.writes[1]["expectedRevision"])
	require.Equal(t, 1, host.signIns, "one session serves the run")
}

func TestEnvelopeWriterReauthenticatesOnlyAnUnauthorizedRequest(t *testing.T) {
	t.Run("expired session", func(t *testing.T) {
		host := newEnvelopeHost(t)
		writer := host.writer()
		writer.accessToken = "expired"
		_, err := writer.Ensure(t.Context(), func(revision uint64) ([]byte, error) { return BuildEnvelope(revision, grantsOf("example")) })
		require.NoError(t, err)
		require.Equal(t, 1, host.signIns)
		require.Len(t, host.writes, 1)
		require.Equal(t, float64(0), host.writes[0]["expectedRevision"], "retry preserves the optimistic concurrency precondition")
	})
	for _, refusal := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(refusal), func(t *testing.T) {
			host := newEnvelopeHost(t)
			host.refusal = refusal
			writer := host.writer()
			_, _, err := writer.Current(t.Context())
			require.Error(t, err)
			wantSignIns := 1
			if refusal == http.StatusUnauthorized {
				wantSignIns = 2
			}
			require.Equal(t, wantSignIns, host.signIns, "retry once only for an unauthenticated request")
			require.Empty(t, host.writes)
		})
	}
}

// admittingHTTPHost answers the manifest route as the saas host does: core's
// admission against the manifest it holds, a withdrawn binding refused with a
// 409 carrying core's wording.
func admittingHTTPHost(t *testing.T) string {
	t.Helper()
	var mu sync.Mutex
	held := map[string]*solutionhost.ModuleManifest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		signed, err := solutionhost.ParseSigned(body)
		require.NoError(t, err)
		manifest, err := solutionhost.ManifestFromVerified(signed.Document)
		require.NoError(t, err)
		mu.Lock()
		defer mu.Unlock()
		if _, err := solutionhost.Admit(manifest.Host.Coordinate, held[manifest.Binding], manifest); err != nil {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "module manifest generation conflicts with this binding's history: " + err.Error()})
			return
		}
		held[manifest.Binding] = manifest
		_ = json.NewEncoder(w).Encode(map[string]any{"disposition": "accepted", "generation": manifest.Generation})
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// Over the legacy HTTP delivery, the saas host's 409 for a binding it holds
// withdrawn is recognized as such, as over the composition interface: a
// refused re-add says so, and a second withdrawal after it is known complete
// rather than refused forever.
func TestALegacyHTTPHostHoldingAWithdrawalIsRecognized(t *testing.T) {
	module, services := nativeModule(t, "crm", false)
	deliverer := &NativeDeliverer{
		Workspace: &resources.Workspace{Name: "example"}, Env: nativeEnvironment(), Signer: nativeSigner(t),
		DeliveryURL: admittingHTTPHost(t), StateDir: t.TempDir(),
		Token: func(string) (string, error) { return "delivery", nil },
	}
	live := renderNative(t, module, services, "api")
	_, err := deliverer.Deliver(t.Context(), live)
	require.NoError(t, err)
	withdrawal, err := WithdrawalOf(live)
	require.NoError(t, err)
	_, err = deliverer.Deliver(t.Context(), withdrawal)
	require.NoError(t, err)

	_, err = deliverer.Deliver(t.Context(), renderNative(t, module, services, "api"))
	require.ErrorIs(t, err, solutionhost.ErrWithdrawn, "a refused re-add is a held withdrawal")
	var refusal *HostStatusError
	require.ErrorAs(t, err, &refusal)
	require.False(t, refusal.Transient())

	again, err := WithdrawalOf(live)
	require.NoError(t, err)
	_, err = deliverer.Deliver(t.Context(), again)
	require.ErrorIs(t, err, solutionhost.ErrWithdrawn, "the second withdrawal meets the one the host holds")

	_, err = (&NativeDeliverer{
		Workspace: deliverer.Workspace, Env: deliverer.Env, Signer: deliverer.Signer, StateDir: t.TempDir(), Token: deliverer.Token,
		DeliveryURL: answeringConflict(t, "module manifest generation is older than the applied generation"),
	}).Deliver(t.Context(), renderNative(t, module, services, "api"))
	require.Error(t, err)
	require.NotErrorIs(t, err, solutionhost.ErrWithdrawn, "another conflict is not a withdrawal")
}

func answeringConflict(t *testing.T, message string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
	}))
	t.Cleanup(server.Close)
	return server.URL
}
