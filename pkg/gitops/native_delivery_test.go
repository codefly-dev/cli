package gitops

import (
	"context"
	"crypto/ed25519"
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
	"google.golang.org/grpc/codes"
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
		started = append(started, NativeIdentity{Module: module, Service: service})
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
		{Service: "api", Container: "api"},
		{Service: "worker", Container: "worker"},
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
	require.ErrorContains(t, err, "no admitted identity workload")
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

// deliveryHost stands in for the host's composition interface: it records
// every carrier, the credential it came with, and answers with the refusal
// queued, if any.
type deliveryHost struct {
	mu        sync.Mutex
	bearers   []string
	manifests []*solutionhost.ModuleManifest
	bodies    [][]byte
	refusals  []error
}

func newDeliveryHost(*testing.T) *deliveryHost { return &deliveryHost{} }

func (h *deliveryHost) prepare(context.Context, string, bool) (*PreparedDelivery, error) {
	return &PreparedDelivery{Content: []byte{0}, Post: h.post}, nil
}

func (h *deliveryHost) post(_ context.Context, _ string, signed *solutionhost.Signed, token string) (Receipt, error) {
	body, err := solutionhost.MarshalSigned(signed)
	if err != nil {
		return Receipt{}, err
	}
	manifest, err := solutionhost.ManifestFromVerified(signed.Document)
	if err != nil {
		return Receipt{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bearers = append(h.bearers, token)
	h.manifests = append(h.manifests, manifest)
	h.bodies = append(h.bodies, body)
	if len(h.refusals) > 0 {
		refusal := h.refusals[0]
		h.refusals = h.refusals[1:]
		return Receipt{}, refusal
	}
	return Receipt{Disposition: "accepted", Generation: manifest.Generation}, nil
}

// deliverer is a native deliverer to the host, its carrier credentials issued
// per module and recorded.
func (h *deliveryHost) deliverer(t *testing.T, state string, modules *[]string) *NativeDeliverer {
	return &NativeDeliverer{
		Workspace: &resources.Workspace{Name: "example"}, Env: nativeEnvironment(), Signer: nativeSigner(t),
		StateDir: state, Prepare: h.prepare,
		Token: func(module string) (string, error) {
			*modules = append(*modules, module)
			return "delivery-" + module, nil
		},
	}
}

// TestANativeGenerationMovesOnlyWhenTheContentDoes: one carrier per delivery,
// with the delivery credential of the workloads' namespace. The same content replays its generation, byte for byte; changed
// content — a second workload started — is the next one; and the record
// outlives the deliverer, so a restarted run replays rather than restarting
// at 1.
func TestANativeGenerationMovesOnlyWhenTheContentDoes(t *testing.T) {
	module, services := nativeModule(t, "crm", false)
	host := newDeliveryHost(t)
	state := t.TempDir()
	var modules []string
	deliverer := host.deliverer(t, state, &modules)

	receipt, err := deliverer.Deliver(t.Context(), renderNative(t, module, services, "api"))
	require.NoError(t, err)
	require.Equal(t, Receipt{Disposition: "accepted", Generation: 1}, receipt)
	_, err = deliverer.Deliver(t.Context(), renderNative(t, module, services, "api"))
	require.NoError(t, err)
	both := renderNative(t, module, services, "api", "worker")
	_, err = deliverer.Deliver(t.Context(), both)
	require.NoError(t, err)
	require.Equal(t, uint64(2), both.Generation, "Deliver stamps the settled generation on the manifest")

	restarted := host.deliverer(t, state, &modules)
	_, err = restarted.Deliver(t.Context(), renderNative(t, module, services, "api", "worker"))
	require.NoError(t, err)

	require.Equal(t, []string{"crm", "crm", "crm", "crm"}, modules)
	require.Equal(t, "delivery-crm", host.bearers[0])
	generations := []uint64{}
	for _, manifest := range host.manifests {
		generations = append(generations, manifest.Generation)
	}
	require.Equal(t, []uint64{1, 1, 2, 2}, generations)
	require.Equal(t, string(host.bodies[0]), string(host.bodies[1]), "unchanged content is the same signed carrier")
	require.FileExists(t, filepath.Join(state, "generations.json"))
}

// A host's refusal (a stale or rewritten generation, a withdrawn binding) is
// an error with its reason, and retains the reservation: delivering the same
// content again settles to the same generation.
func TestANativeDeliveryTheHostRefusesRetainsItsReservation(t *testing.T) {
	module, services := nativeModule(t, "crm", false)
	host := newDeliveryHost(t)
	host.refusals = []error{&HostStatusError{Code: codes.FailedPrecondition, Message: "module manifest generation is older than the applied generation"}}
	var modules []string
	deliverer := host.deliverer(t, t.TempDir(), &modules)
	_, err := deliverer.Deliver(t.Context(), renderNative(t, module, services, "api"))
	require.ErrorContains(t, err, "older than the applied generation")
	_, err = deliverer.Deliver(t.Context(), renderNative(t, module, services, "api"))
	require.NoError(t, err)
	require.Equal(t, uint64(1), host.manifests[1].Generation, "the refused delivery retained its reserved generation")
}

// TestANativeWithdrawalIsARemovedManifestAtTheNextGeneration: what withdraws
// a module a native run delivered is its manifest's identity alone, removed,
// at the next generation — the same withdrawal a publication settles —
// delivered with the credential of the module's namespace, and replayed as it
// was until the host has applied it.
func TestANativeWithdrawalIsARemovedManifestAtTheNextGeneration(t *testing.T) {
	module, services := nativeModule(t, "crm", false)
	host := newDeliveryHost(t)
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
