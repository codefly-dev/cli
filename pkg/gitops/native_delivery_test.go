package gitops

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

// nativeModule lays down what a native delivery of a module reads: its
// package manifest, and — with a page — the frontend beside its entry
// service. Its services are api and worker, both running as the module, and
// docs, which does not.
func nativeModule(t *testing.T, name string, page bool) (*resources.Module, []*resources.Service) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "services", "api"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "module.package.codefly.yaml"), []byte("kind: module-package\nschema: codefly/module-package/v2\nid: example/"+name+"\nversion: 1.0.0\nminimum-codefly-version: '>=0.0.0'\nartifact-roots: [services]\ncontracts:\n  composition: '>=1.0'\n"), 0o600))
	module := &resources.Module{Name: name}
	if page {
		module.ServiceEntry = "api"
		require.NoError(t, os.WriteFile(filepath.Join(dir, "services", "api", solutionrun.FrontendFileName), []byte("title: Page\norder: 1\nexposed_module: ./Page\nhost_contract: 1\n"), 0o600))
	}
	module.WithDir(dir)
	return module, []*resources.Service{
		{Name: "api", ModuleIdentity: true, Endpoints: []*resources.Endpoint{{Name: "http", API: "http", Visibility: "internal"}}},
		{Name: "worker", ModuleIdentity: true, Endpoints: []*resources.Endpoint{{Name: "grpc", API: "grpc", Visibility: "internal"}}},
		{Name: "docs", Endpoints: []*resources.Endpoint{{Name: "http", API: "http", Visibility: "internal"}}},
	}
}

func nativeEnvironment() *environments.Environment {
	return &environments.Environment{Name: "prod", Namespace: "example", Host: testHost()}
}

// nativeStarted are a module's identity workloads as a native run starts
// them, in the module's namespace.
func nativeStarted(module string, services ...string) []NativeIdentity {
	started := make([]NativeIdentity, 0, len(services))
	for _, service := range services {
		started = append(started, NativeIdentity{Module: module, Service: service, Account: service, SPIFFEID: testHost().SPIFFEID("example-"+module, service)})
	}
	return started
}

// nativeCarriers are the addresses a native run resolved for the module's
// routed endpoints, keyed as the gateway reads them.
func nativeCarriers(module string) map[string]string {
	key := func(service, name, api string) string {
		return resources.EndpointAsEnvironmentVariableKey(&resources.EndpointInformation{Module: module, Service: service, Name: name, API: api})
	}
	return map[string]string{key("api", "http", "http"): "http://127.0.0.1:50695", key("worker", "grpc", "grpc"): "127.0.0.1:50696"}
}

func nativeSigner(t *testing.T) solutionhost.Signer {
	t.Helper()
	signer, err := solutionhost.NewInMemorySigner("dev-0123456789abcdef", ed25519.NewKeyFromSeed(make([]byte, 32)))
	require.NoError(t, err)
	return signer
}

func renderNative(t *testing.T, module *resources.Module, services []*resources.Service, started ...string) *solutionhost.ModuleManifest {
	t.Helper()
	manifest, err := (&NativeDeliverer{Workspace: &resources.Workspace{Name: "example"}, Env: nativeEnvironment()}).Render(t.Context(), &NativeWorkload{
		Module: module, Services: services, Started: nativeStarted(module.Name, started...), Carriers: nativeCarriers(module.Name),
	})
	require.NoError(t, err)
	return manifest
}

// TestANativeManifestListsEveryStartedIdentityWorkload: a module runs as each
// of its identity services, so the manifest a native run delivers lists every
// one it started — two here, one manifest — each with the account a cell
// would give it, and the endpoints of exactly those, at the addresses the run
// allocated. A workload not started yet routes nothing yet.
func TestANativeManifestListsEveryStartedIdentityWorkload(t *testing.T) {
	module, services := nativeModule(t, "crm", false)
	manifest := renderNative(t, module, services, "worker", "api")
	require.Equal(t, "example.prod.crm", manifest.Binding)
	require.Equal(t, solutionhost.KindModule, manifest.Kind)
	require.Equal(t, solutionhost.Release{Publisher: "example", Name: "crm", Version: "1.0.0"}, manifest.Release)
	require.Equal(t, []solutionhost.Workload{
		{Service: "api", Container: "api", SPIFFEID: "spiffe://cluster.example/ns/example-crm/sa/api"},
		{Service: "worker", Container: "worker", SPIFFEID: "spiffe://cluster.example/ns/example-crm/sa/worker"},
	}, manifest.Workloads)
	require.ElementsMatch(t, []solutionhost.Endpoint{
		{Name: "http", Service: "api", API: "http", Address: "http://127.0.0.1:50695"},
		{Name: "grpc", Service: "worker", API: "grpc", Address: "127.0.0.1:50696"},
	}, manifest.Endpoints)
	require.Equal(t, &solutionhost.Authority{Principal: "crm"}, manifest.Authority, "a module with workloads always runs as its principal")

	one := renderNative(t, module, services, "api")
	require.Len(t, one.Workloads, 1)
	require.Equal(t, []solutionhost.Endpoint{{Name: "http", Service: "api", API: "http", Address: "http://127.0.0.1:50695"}}, one.Endpoints)
}

// A host routes only to a workload that runs as the module: an endpoint the
// run routes for any other service is refused by name, and a started service
// that does not declare module-identity is no workload of the module.
func TestANativeManifestRoutesOnlyToTheModulesWorkloads(t *testing.T) {
	module, services := nativeModule(t, "crm", false)
	carriers := nativeCarriers("crm")
	carriers[resources.EndpointAsEnvironmentVariableKey(&resources.EndpointInformation{Module: "crm", Service: "docs", Name: "http", API: "http"})] = "http://127.0.0.1:50697"
	deliverer := &NativeDeliverer{Workspace: &resources.Workspace{Name: "example"}, Env: nativeEnvironment()}
	_, err := deliverer.Render(t.Context(), &NativeWorkload{Module: module, Services: services, Started: nativeStarted("crm", "api"), Carriers: carriers})
	require.ErrorContains(t, err, "service docs endpoint http is routed by the host")

	_, err = deliverer.Render(t.Context(), &NativeWorkload{Module: module, Services: services, Started: nativeStarted("crm", "docs"), Carriers: nativeCarriers("crm")})
	require.ErrorContains(t, err, "does not declare module-identity")

	_, err = deliverer.Render(t.Context(), &NativeWorkload{Module: module, Services: services})
	require.ErrorContains(t, err, "no started identity workload")
}

// A native run delivers the bundle it built: the payload names it, a page
// with no built bundle is refused, and so is a bundle with no page.
func TestANativeManifestNamesTheBundleTheRunBuilt(t *testing.T) {
	bundle := &solutionhost.FrontendPayload{Artifact: FrontendArtifactName, Digest: solutionhost.PayloadDigest("sha256:" + strings.Repeat("e", 64)), MediaType: "application/x-tar"}
	deliverer := &NativeDeliverer{Workspace: &resources.Workspace{Name: "example"}, Env: nativeEnvironment()}
	page, services := nativeModule(t, "crm", true)
	manifest, err := deliverer.Render(t.Context(), &NativeWorkload{Module: page, Services: services, Started: nativeStarted("crm", "api"), FrontendBundle: bundle})
	require.NoError(t, err)
	require.NotNil(t, manifest.Frontend)
	require.Equal(t, bundle, manifest.Frontend.Payload)

	_, err = deliverer.Render(t.Context(), &NativeWorkload{Module: page, Services: services, Started: nativeStarted("crm", "api")})
	require.ErrorContains(t, err, "built no bundle")

	headless, services := nativeModule(t, "wiki", false)
	_, err = deliverer.Render(t.Context(), &NativeWorkload{Module: headless, Services: services, Started: nativeStarted("wiki", "api"), FrontendBundle: bundle})
	require.ErrorContains(t, err, "declares no frontend")
}

// deliveryHost stands in for the host's delivery listener: it records every
// carrier, the credential it came with, and answers with the status queued.
type deliveryHost struct {
	*httptest.Server
	mu        sync.Mutex
	paths     []string
	bearers   []string
	manifests []*solutionhost.ModuleManifest
	bodies    [][]byte
	status    []int
}

func newDeliveryHost(t *testing.T) *deliveryHost {
	t.Helper()
	host := &deliveryHost{}
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

// deliverer is a native deliverer to the host, its credentials issued per
// namespace and recorded.
func (h *deliveryHost) deliverer(t *testing.T, state string, namespaces *[]string) *NativeDeliverer {
	return &NativeDeliverer{
		Workspace: &resources.Workspace{Name: "example"}, Env: nativeEnvironment(), Signer: nativeSigner(t),
		DeliveryURL: h.URL, StateDir: state,
		Token: func(namespace string) (string, error) {
			*namespaces = append(*namespaces, namespace)
			return "delivery-" + namespace, nil
		},
	}
}

// TestANativeGenerationMovesOnlyWhenTheContentDoes: one carrier per delivery,
// to the manifest route, with the delivery credential of the workloads'
// namespace. The same content replays its generation, byte for byte; changed
// content — a second workload started — is the next one; and the record
// outlives the deliverer, so a restarted run replays rather than restarting
// at 1.
func TestANativeGenerationMovesOnlyWhenTheContentDoes(t *testing.T) {
	module, services := nativeModule(t, "crm", false)
	host := newDeliveryHost(t)
	state := t.TempDir()
	var namespaces []string
	deliverer := host.deliverer(t, state, &namespaces)

	receipt, err := deliverer.Deliver(t.Context(), renderNative(t, module, services, "api"), "example-crm")
	require.NoError(t, err)
	require.Equal(t, Receipt{Status: http.StatusOK, Disposition: "accepted", Generation: 1}, receipt)
	_, err = deliverer.Deliver(t.Context(), renderNative(t, module, services, "api"), "example-crm")
	require.NoError(t, err)
	both := renderNative(t, module, services, "api", "worker")
	_, err = deliverer.Deliver(t.Context(), both, "example-crm")
	require.NoError(t, err)
	require.Equal(t, uint64(2), both.Generation, "Deliver stamps the settled generation on the manifest")

	restarted := host.deliverer(t, state, &namespaces)
	_, err = restarted.Deliver(t.Context(), renderNative(t, module, services, "api", "worker"), "example-crm")
	require.NoError(t, err)

	require.Equal(t, []string{manifestDeliveryPath, manifestDeliveryPath, manifestDeliveryPath, manifestDeliveryPath}, host.paths)
	require.Equal(t, []string{"example-crm", "example-crm", "example-crm", "example-crm"}, namespaces)
	require.Equal(t, "Bearer delivery-example-crm", host.bearers[0])
	generations := []uint64{}
	for _, manifest := range host.manifests {
		generations = append(generations, manifest.Generation)
	}
	require.Equal(t, []uint64{1, 1, 2, 2}, generations)
	require.Equal(t, string(host.bodies[0]), string(host.bodies[1]), "unchanged content is the same signed carrier")
	require.FileExists(t, filepath.Join(state, "generations.json"))
}

// A host's refusal (409: a stale or rewritten generation, a withdrawn
// binding) is an error with its reason, and records nothing: delivering the
// same content again settles to the same generation.
func TestANativeDeliveryTheHostRefusesIsAnErrorAndRecordsNothing(t *testing.T) {
	module, services := nativeModule(t, "crm", false)
	host := newDeliveryHost(t)
	host.status = []int{http.StatusConflict}
	var namespaces []string
	deliverer := host.deliverer(t, t.TempDir(), &namespaces)
	receipt, err := deliverer.Deliver(t.Context(), renderNative(t, module, services, "api"), "example-crm")
	require.ErrorContains(t, err, "older than the applied generation")
	require.Equal(t, http.StatusConflict, receipt.Status)
	_, err = deliverer.Deliver(t.Context(), renderNative(t, module, services, "api"), "example-crm")
	require.NoError(t, err)
	require.Equal(t, uint64(1), host.manifests[1].Generation, "the refused delivery recorded no generation")
}

// TestANativeWithdrawalIsARemovedManifestAtTheNextGeneration: what withdraws
// a module a native run delivered is its manifest's identity alone, removed,
// at the next generation — the same withdrawal a publication settles —
// delivered with the credential of the module's namespace, and replayed as it
// was until the host has applied it.
func TestANativeWithdrawalIsARemovedManifestAtTheNextGeneration(t *testing.T) {
	module, services := nativeModule(t, "crm", false)
	host := newDeliveryHost(t)
	var namespaces []string
	deliverer := host.deliverer(t, t.TempDir(), &namespaces)
	live := renderNative(t, module, services, "api", "worker")
	_, err := deliverer.Deliver(t.Context(), live, "example-crm")
	require.NoError(t, err)

	withdrawal, err := WithdrawalOf(live)
	require.NoError(t, err)
	_, err = deliverer.Deliver(t.Context(), withdrawal, "example-crm")
	require.NoError(t, err)
	replay, err := WithdrawalOf(live)
	require.NoError(t, err)
	_, err = deliverer.Deliver(t.Context(), replay, "example-crm")
	require.NoError(t, err)

	posted := host.manifests[1]
	require.True(t, posted.Removed)
	require.Equal(t, uint64(2), posted.Generation)
	require.Equal(t, live.Binding, posted.Binding)
	require.Empty(t, posted.Workloads)
	require.Empty(t, posted.Endpoints)
	require.Nil(t, posted.Authority)
	require.Equal(t, []string{"example-crm", "example-crm", "example-crm"}, namespaces, "a withdrawal is delivered in the module's namespace")
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
