package gitops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// stageRenderNamingEndpoints renders the crm instance as a composed render
// does — its presence names the grpc endpoint of its api unit — and records,
// as the render would, the in-cluster carrier of that endpoint at the given
// address, beside one of an endpoint no presence names.
func stageRenderNamingEndpoints(t *testing.T, repository *deliveryRepository, address string) (*Inventory, map[string]string) {
	t.Helper()
	require.NoError(t, os.RemoveAll(repository.target))
	options := solutionRenderOptions(repository.target)
	result, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment))
	require.NoError(t, err)
	inventory := result.Inventory
	key := resources.EndpointAsEnvironmentVariableKey(&resources.EndpointInformation{Module: "crm", Service: "api", Name: "grpc", API: "grpc"})
	inventory.EndpointCarriers = map[string]string{key: address, "CODEFLY__ENDPOINT__OTHER__API__REST__REST": "http://other.svc:1"}
	return &inventory, map[string]string{key: address}
}

func documentOfKind(t *testing.T, delivery *InventoryDelivery, kind, id string) InventoryDeliveredDocument {
	t.Helper()
	for _, document := range delivery.Documents {
		if document.Kind == kind && document.ID == id {
			return document
		}
	}
	t.Fatalf("the delivery records no %s document for %s: %+v", kind, id, delivery.Documents)
	return InventoryDeliveredDocument{}
}

// deliveredEndpointsOf reads the signed endpoint delivery carried beside a
// settled presence and joins it to that presence as the host does.
func deliveredEndpointsOf(t *testing.T, repository *deliveryRepository, binding string) (string, map[string]string) {
	t.Helper()
	carrier := settledDocuments(t, repository)[binding]
	require.Contains(t, carrier.Data, presenceCarrierKey)
	raw := carrier.Data[endpointCarrierKey]
	signed, err := solutionhost.ParseSigned([]byte(raw))
	require.NoError(t, err, "the endpoint delivery is carried signed beside the presence")
	parsed, err := solutionhost.ParseEndpointDelivery(signed.Document, readDeliveredPresence(t, repository.target, "prod", binding))
	require.NoError(t, err, "bound to the settled presence: generation and digest")
	return raw, parsed.Carriers
}

// A publication delivers, beside each settled presence that names endpoints,
// the carriers the render recorded for them, bound to the settled generation
// and digest, signed as the presence was, carried in the same ConfigMap and
// posted by their own Job through the same inbox after it. The addresses are
// settled with the generation: an unchanged publish reuses the delivered
// carrier byte for byte, an address that moved under an unchanged presence
// is the next generation of both, and a delivered carrier that signs other
// bytes is refused as a presence carrier is.
func TestPublishDeliversTheEndpointsOfEachSettledPresence(t *testing.T) {
	ctx := context.Background()
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{}
	opts := deliveryPublishOptions{Signer: signer, Target: testDeliveryTarget(), Domain: "example", EnvelopeRevision: 1, Module: "crm"}
	settle := func(inventory *Inventory) (*InventoryDelivery, error) {
		return settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, &opts)
	}
	const binding = "example.prod.crm"

	inventory, named := stageRenderNamingEndpoints(t, repository, "http://api.crm.svc.cluster.local:9090")
	delivery, err := settle(inventory)
	require.NoError(t, err)
	require.True(t, delivery.Signed)
	require.Equal(t, 2, signer.signed, "the presence and its endpoints, one signature each")
	endpoints := documentOfKind(t, delivery, deliveredEndpoints, binding)
	require.EqualValues(t, 1, endpoints.Generation)
	require.True(t, strings.HasPrefix(endpoints.Digest, "sha256:"))
	first, carriers := deliveredEndpointsOf(t, repository, binding)
	require.Equal(t, named, carriers, "the endpoints the presence names, and no other")

	overlay := filepath.Join(repository.target, filepath.FromSlash(solutionHostBindingOverlay("prod")))
	job, err := os.ReadFile(filepath.Join(overlay, "deliver-"+deliveryEndpoints+".yaml"))
	require.NoError(t, err, "an endpoints Job delivers them after the presence through the same inbox")
	for _, want := range []string{endpointsDeliveryPath, endpointCarrierKey, solutionHostBindingKind + "-" + binding} {
		require.Contains(t, string(job), want)
	}
	kustomization, err := os.ReadFile(filepath.Join(overlay, kustomizationFile))
	require.NoError(t, err)
	require.Contains(t, string(kustomization), "deliver-"+deliveryEndpoints)
	repository.deliver(t)

	// Nothing changed: both carriers are the delivered ones, byte for byte,
	// and nothing is signed again.
	inventory, _ = stageRenderNamingEndpoints(t, repository, "http://api.crm.svc.cluster.local:9090")
	delivery, err = settle(inventory)
	require.NoError(t, err)
	require.Equal(t, 2, signer.signed, "an unchanged delivery is not signed again")
	require.EqualValues(t, 1, documentOfKind(t, delivery, deliveredEndpoints, binding).Generation)
	again, _ := deliveredEndpointsOf(t, repository, binding)
	require.Equal(t, first, again, "the endpoint carrier is byte-identical")
	repository.deliver(t)

	// An address moved under an unchanged presence: the next generation of
	// both, since the host keeps one endpoint delivery per applied
	// generation and refuses other bytes at the same one.
	inventory, moved := stageRenderNamingEndpoints(t, repository, "http://api.crm.svc.cluster.local:9091")
	delivery, err = settle(inventory)
	require.NoError(t, err)
	require.Equal(t, 4, signer.signed, "the presence and its endpoints are signed again at the next generation")
	require.EqualValues(t, 2, documentOfKind(t, delivery, deliveredPresence, binding).Generation, "the unchanged presence moves with its addresses")
	require.EqualValues(t, 2, documentOfKind(t, delivery, deliveredEndpoints, binding).Generation)
	_, carriers = deliveredEndpointsOf(t, repository, binding)
	require.Equal(t, moved, carriers)
	repository.deliver(t)

	// The delivered endpoint carrier's signed bytes are altered in place: the
	// delivery an unchanged publish would reuse signs other bytes, so the
	// publish is refused rather than republishing it as signed.
	path := filepath.Join(overlay, binding+".yaml")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var carrier solutionHostBindingConfigMap
	require.NoError(t, yaml.Unmarshal(data, &carrier))
	signed, err := solutionhost.ParseSigned([]byte(carrier.Data[endpointCarrierKey]))
	require.NoError(t, err)
	altered := bytes.Replace(signed.Document, []byte(`"generation":2`), []byte(`"generation":7`), 1)
	require.NotEqual(t, string(signed.Document), string(altered), "the delivery names the generation it is bound to")
	signed.Document = altered
	tampered, err := solutionhost.MarshalSigned(signed)
	require.NoError(t, err)
	carrier.Data[endpointCarrierKey] = string(tampered)
	data, err = yaml.Marshal(carrier)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	repository.deliver(t)
	inventory, _ = stageRenderNamingEndpoints(t, repository, "http://api.crm.svc.cluster.local:9091")
	_, err = settle(inventory)
	require.ErrorContains(t, err, "signs other bytes")
	require.Equal(t, 4, signer.signed, "nothing is signed over a tree that disagrees with its carrier")
}

// A presence naming no endpoint delivers none: no carrier beside it, no Job,
// nothing recorded, one signature for the presence alone.
func TestAPresenceNamingNoEndpointDeliversNone(t *testing.T) {
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{}
	opts := deliveryPublishOptions{Signer: signer, Target: testDeliveryTarget(), Domain: "example", Module: "crm"}
	delivery, err := settlePresenceDelivery(context.Background(), repository.repo, "main", repository.target, repository.targetPath, "prod", repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	require.Equal(t, 1, signer.signed)
	for _, document := range delivery.Documents {
		require.NotEqual(t, deliveredEndpoints, document.Kind, "%+v", delivery.Documents)
	}
	require.NotContains(t, settledDocuments(t, repository)["example.prod.crm"].Data, endpointCarrierKey)
	_, err = os.Stat(filepath.Join(repository.target, filepath.FromSlash(solutionHostBindingOverlay("prod")), "deliver-"+deliveryEndpoints+".yaml"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// The carriers a delivery carries are exactly the ones of the endpoints the
// presence names, selected by the carrier key the gateway reads them under.
func TestADeliveryCarriesOnlyTheEndpointsThePresenceNames(t *testing.T) {
	document := &solutionhost.SolutionHostBinding{Endpoints: []solutionhost.Endpoint{
		{Name: "http", Service: "backend", Module: "wiki", API: "http", Visibility: "internal"},
	}}
	wiki := resources.EndpointAsEnvironmentVariableKey(&resources.EndpointInformation{Module: "wiki", Service: "backend", Name: "http", API: "http"})
	recorded := map[string]string{wiki: "http://backend.wiki.svc.cluster.local:8080", "CODEFLY__ENDPOINT__DOCS__DOCUMENTS__WEB__HTTP": "http://documents.docs.svc.cluster.local:8080"}
	require.Equal(t, map[string]string{wiki: recorded[wiki]}, endpointCarriersOf(document, recorded))
	require.Empty(t, endpointCarriersOf(&solutionhost.SolutionHostBinding{}, recorded))
}
