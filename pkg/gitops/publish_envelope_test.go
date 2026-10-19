package gitops

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/delivery/signing"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// envelopeOptions are publishOptions toward testHost, held to the release
// policy a hosted publish runs, with the composition stating principals.
func envelopeOptions(t *testing.T, signer *fakeSigner, principals map[string]json.RawMessage) deliveryPublishOptions {
	t.Helper()
	check, err := signing.KeyCheck(signer, solutionhost.KeyKindRelease, "example", signer.KeyID())
	require.NoError(t, err)
	opts := publishOptions(signer)
	opts.Coordinate = testHost().Coordinate
	opts.SelfCheck, opts.ReuseCheck = check, check
	opts.Envelope = &declaredEnvelope{Principals: principals, Stated: principals != nil, StatesPrincipals: principals != nil}
	return opts
}

func recordsGrant(audience string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"records": json.RawMessage(`{"operation_audiences":{"` + audience + `":{"audience":"` + audience + `"}}}`)}
}

// deliveredEnvelope reads the envelope ConfigMap the settlement wrote.
func deliveredEnvelope(t *testing.T, repository *deliveryRepository) moduleManifestConfigMap {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repository.target, filepath.FromSlash(moduleManifestOverlay("prod")), moduleEnvelopeFile))
	require.NoError(t, err)
	var configMap moduleManifestConfigMap
	require.NoError(t, yaml.Unmarshal(data, &configMap))
	return configMap
}

// verifyAsTheHost verifies the delivered carrier with core's host-side check,
// against the signer pinned as a release key for the domain.
func verifyAsTheHost(t *testing.T, signer *fakeSigner, carrier string) *solutionhost.ModuleEnvelope {
	t.Helper()
	public, err := signing.PublicKeyPEM(signer)
	require.NoError(t, err)
	keys, err := solutionhost.NewPinnedKeySet(solutionhost.PinnedKey{KeyID: signer.KeyID(), PublicKeyPEM: public, Kind: solutionhost.KeyKindRelease, OwnershipDomains: []string{"example"}})
	require.NoError(t, err)
	verifier, err := solutionhost.NewVerifier(keys)
	require.NoError(t, err)
	signed, err := solutionhost.ParseSigned([]byte(carrier))
	require.NoError(t, err)
	delivered, err := solutionhost.VerifyDeliveredEnvelope(context.Background(), signed, verifier)
	require.NoError(t, err)
	envelope, err := delivered.Envelope()
	require.NoError(t, err)
	return envelope
}

// TestPublishDeliversTheModuleEnvelopeLikeAManifest: the first envelope is
// revision 1, signed and verifiable as its host verifies it, with a Job that
// POSTs it to the envelope route as the namespace's admitted carrier; an
// unchanged republish keeps the revision and the carrier and signs nothing;
// changed grants are the next revision.
func TestPublishDeliversTheModuleEnvelopeLikeAManifest(t *testing.T) {
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{}
	opts := envelopeOptions(t, signer, recordsGrant("tracing"))

	delivery, err := repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	require.True(t, delivery.Signed)
	require.Equal(t, 2, signer.signed, "the manifest and the envelope")
	document := documentByID(delivery, deliveredEnvelopeID)
	require.Equal(t, deliveredEnvelopeKind, document.Kind)
	require.Equal(t, uint64(1), document.Generation)
	configMap := deliveredEnvelope(t, repository)
	envelope := verifyAsTheHost(t, signer, configMap.Data[solutionhost.EnvelopeSignedFileName])
	require.Equal(t, uint64(1), envelope.Revision)
	require.Equal(t, solutionhost.HostTarget{Coordinate: testHost().Coordinate, Domain: "example"}, envelope.Host)
	digest, err := envelope.Digest()
	require.NoError(t, err)
	require.Equal(t, digest, document.Digest)
	_, err = solutionhost.EnvelopeFromVerified([]byte(configMap.Data[moduleEnvelopeDocument]))
	require.NoError(t, err, "the readable document is the canonical one")
	require.NotEmpty(t, opts.Carriers[carrierKey(deliveredEnvelopeKind, deliveredEnvelopeID, 1)], "a plan hands the carrier to the publish executing it")

	overlay := filepath.Join(repository.target, filepath.FromSlash(moduleManifestOverlay("prod")))
	kustomization, err := os.ReadFile(filepath.Join(overlay, kustomizationFile))
	require.NoError(t, err)
	for _, want := range []string{moduleEnvelopeFile, "deliver-envelope.yaml", "deliver-manifest.yaml", deliveryServiceAccountFile} {
		require.Contains(t, string(kustomization), want)
	}
	jobBytes, err := os.ReadFile(filepath.Join(overlay, "deliver-envelope.yaml"))
	require.NoError(t, err)
	var job deliveryJobManifest
	require.NoError(t, yaml.Unmarshal(jobBytes, &job))
	require.Equal(t, deliveredEnvelopeKind, job.Metadata.Labels[deliveredDocumentLabel])
	require.Equal(t, deliveryCarrierValue, job.Metadata.Labels[deliveryLabel], "the Job is selected as the carrier too")
	require.Equal(t, deliveryCarrierOf().Labels, job.Spec.Template.Metadata.Labels, "the pod is the carrier the cell admits")
	require.Equal(t, "crm", job.Metadata.Namespace)
	for _, want := range []string{"value: " + envelopeDeliveryPath, "key: " + solutionhost.EnvelopeSignedFileName, "name: " + moduleEnvelopeConfigMap, "serviceAccountName: delivery"} {
		require.Contains(t, string(jobBytes), want)
	}
	repository.deliver(t)

	// Unchanged: the same revision, the same carrier, nothing signed.
	before := configMap.Data[solutionhost.EnvelopeSignedFileName]
	delivery, err = repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	require.Equal(t, 2, signer.signed, "an unchanged envelope is not signed again")
	require.Equal(t, uint64(1), documentByID(delivery, deliveredEnvelopeID).Generation)
	require.Equal(t, before, deliveredEnvelope(t, repository).Data[solutionhost.EnvelopeSignedFileName])
	repository.deliver(t)

	// Changed grants: the next revision, signed anew.
	changed := envelopeOptions(t, signer, recordsGrant("annotations"))
	delivery, err = repository.settle(repository.stageRender(t, "crm"), &changed)
	require.NoError(t, err)
	require.Equal(t, 3, signer.signed)
	require.Equal(t, uint64(2), documentByID(delivery, deliveredEnvelopeID).Generation)
	require.Equal(t, uint64(2), verifyAsTheHost(t, signer, deliveredEnvelope(t, repository).Data[solutionhost.EnvelopeSignedFileName]).Revision)
}

// TestPublishRefusesADeliveredEnvelopeGrantNamingATenant: a delivered grant
// names no organization; the host refuses one, so publish does first.
func TestPublishRefusesADeliveredEnvelopeGrantNamingATenant(t *testing.T) {
	repository := newDeliveryRepository(t)
	opts := envelopeOptions(t, &fakeSigner{}, map[string]json.RawMessage{
		"records": json.RawMessage(`{"tenant":"00000000-0000-7000-8000-0000000000c1"}`),
		"compute": json.RawMessage(`{"tenant":""}`),
	})
	_, err := repository.settle(repository.stageRender(t, "crm"), &opts)
	require.ErrorContains(t, err, "grants records with a tenant")
	require.ErrorContains(t, err, "names no organization")
	require.NotContains(t, err.Error(), "compute", "an empty tenant names nothing")
}

// TestPublishRefusesAnEnvelopeTheCompositionStoppedStating: an envelope
// delivered before and stated by nothing now is refused, never skipped;
// stating {"principals": {}} revokes every grant at the next revision.
func TestPublishRefusesAnEnvelopeTheCompositionStoppedStating(t *testing.T) {
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{}
	opts := envelopeOptions(t, signer, recordsGrant("tracing"))
	_, err := repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	repository.deliver(t)

	silent := envelopeOptions(t, signer, nil)
	_, err = repository.settle(repository.stageRender(t, "crm"), &silent)
	require.ErrorContains(t, err, `state {"principals": {}}`)

	unhosted := envelopeOptions(t, signer, nil)
	unhosted.Envelope = nil
	_, err = repository.settle(repository.stageRender(t, "crm"), &unhosted)
	require.ErrorContains(t, err, "delivered the module envelope (revision 1)")

	revoked := envelopeOptions(t, signer, map[string]json.RawMessage{})
	delivery, err := repository.settle(repository.stageRender(t, "crm"), &revoked)
	require.NoError(t, err)
	require.Equal(t, uint64(2), documentByID(delivery, deliveredEnvelopeID).Generation)
	require.Empty(t, verifyAsTheHost(t, signer, deliveredEnvelope(t, repository).Data[solutionhost.EnvelopeSignedFileName]).Principals)
}

// TestPublishRefusesAStatedEnvelopeThatStatesNoPrincipals: the file existing
// is not the statement "grants nothing". A composition that writes
// module-envelope.json for the floor alone — the shape the 409 remedy walks an
// operator toward — states no principals, and signing its empty merge under
// release custody would revoke every grant on the cell and stop every module
// principal minting. Only an authored {} revokes.
func TestPublishRefusesAStatedEnvelopeThatStatesNoPrincipals(t *testing.T) {
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{}

	floorOnly := envelopeOptions(t, signer, nil)
	floorOnly.Envelope = &declaredEnvelope{Stated: true, MinimumRevision: 12}
	_, err := repository.settle(repository.stageRender(t, "crm"), &floorOnly)
	require.ErrorContains(t, err, "states no principals")
	require.ErrorContains(t, err, `state {"principals": {...}}`)

	// The same refusal when the key was stated but read as absent, so no
	// reading of the file reaches the signature as a revocation.
	dropped := envelopeOptions(t, signer, nil)
	dropped.Envelope = &declaredEnvelope{Stated: true, StatesPrincipals: true}
	_, err = repository.settle(repository.stageRender(t, "crm"), &dropped)
	require.ErrorContains(t, err, "states no principals")
}

// TestPublishDeliversNoEnvelopeNobodyStates: no declaration (another module,
// no host, local qualification) and no file in the composition both deliver
// nothing when nothing was delivered before.
func TestPublishDeliversNoEnvelopeNobodyStates(t *testing.T) {
	for name, envelope := range map[string]*declaredEnvelope{"not declared": nil, "not stated": {Stated: false}} {
		t.Run(name, func(t *testing.T) {
			repository := newDeliveryRepository(t)
			opts := envelopeOptions(t, &fakeSigner{}, nil)
			opts.Envelope = envelope
			delivery, err := repository.settle(repository.stageRender(t, "crm"), &opts)
			require.NoError(t, err)
			require.Empty(t, documentByID(delivery, deliveredEnvelopeID).ID)
			_, err = os.Stat(filepath.Join(repository.target, filepath.FromSlash(moduleManifestOverlay("prod")), moduleEnvelopeFile))
			require.True(t, os.IsNotExist(err))
		})
	}
}

// TestPublishDeliversTheEnvelopeOfAHostWithNoManifest: the envelope alone
// makes the delivery overlay — its account, its Job, its kustomization.
func TestPublishDeliversTheEnvelopeOfAHostWithNoManifest(t *testing.T) {
	repository := newDeliveryRepository(t)
	opts := envelopeOptions(t, &fakeSigner{}, recordsGrant("tracing"))
	inventory := repository.stageRender(t, "")
	delivery, err := repository.settle(inventory, &opts)
	require.NoError(t, err)
	require.Len(t, delivery.Documents, 1)
	require.Equal(t, moduleManifestDir, inventory.ModuleManifestPath)
	kustomization, err := os.ReadFile(filepath.Join(repository.target, filepath.FromSlash(moduleManifestOverlay("prod")), kustomizationFile))
	require.NoError(t, err)
	for _, want := range []string{moduleEnvelopeFile, "deliver-envelope.yaml", deliveryServiceAccountFile} {
		require.Contains(t, string(kustomization), want)
	}
}

// TestPublishRefusesAnUnsignedEnvelopeOutsideLocalQualification: like a
// manifest, an envelope the signer cannot sign is refused by name, and only a
// publish allowed to deliver unsigned writes it, with no Job to POST it.
func TestPublishRefusesAnUnsignedEnvelopeOutsideLocalQualification(t *testing.T) {
	repository := newDeliveryRepository(t)
	inventory := repository.stageRender(t, "")
	opts := publishOptions(signing.FromEnvironment(func(string) (string, bool) { return "", false }))
	opts.Coordinate = testHost().Coordinate
	opts.Envelope = &declaredEnvelope{Principals: recordsGrant("tracing"), Stated: true, StatesPrincipals: true}
	_, err := repository.settle(inventory, &opts)
	require.ErrorIs(t, err, signing.ErrNoIdentity)
	require.ErrorContains(t, err, "the module envelope cannot be delivered unsigned to prod")
	opts.AllowUnsigned = true
	delivery, err := repository.settle(inventory, &opts)
	require.NoError(t, err)
	require.False(t, delivery.Signed)
	require.NotContains(t, deliveredEnvelope(t, repository).Data, solutionhost.EnvelopeSignedFileName)
	_, err = os.Stat(filepath.Join(repository.target, filepath.FromSlash(moduleManifestOverlay("prod")), "deliver-envelope.yaml"))
	require.True(t, os.IsNotExist(err), "nothing POSTs an envelope the host refuses")
}

// TestPublishRefusesAnEnvelopeSignedByAPersonKey: the self-check holds an
// envelope to a release key, as its host does.
func TestPublishRefusesAnEnvelopeSignedByAPersonKey(t *testing.T) {
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{}
	opts := envelopeOptions(t, signer, recordsGrant("tracing"))
	person, err := signing.KeyCheck(signer, solutionhost.KeyKindPerson, "example", signer.KeyID())
	require.NoError(t, err)
	opts.SelfCheck, opts.ReuseCheck = person, person
	_, err = repository.settle(repository.stageRender(t, ""), &opts)
	require.ErrorIs(t, err, solutionhost.ErrEnvelopeSignerNotRelease)
}

// TestOnlyTheHostingModulesHostedPublishDeclaresTheEnvelope: the envelope is
// read from the composition only for the module that hosts the delivery API,
// toward a declared host, outside local qualification.
func TestOnlyTheHostingModulesHostedPublishDeclaresTheEnvelope(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte("name: acme\nlayout: modules\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "configurations", "prod"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "configurations", "prod", "module-envelope.json"), []byte(`{"principals":{"records":{"operation_audiences":{}}},"minimum_revision":3}`), 0o644))
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	require.NoError(t, err)

	hosting := &Inventory{Module: "platform", HostsDelivery: true}
	for name, publication := range map[string]*deliveryPublication{
		"hosted":     {workspace: workspace, options: deliveryPublishOptions{Target: testDeliveryTarget()}},
		"no host":    {workspace: workspace},
		"local":      {workspace: workspace, local: true, options: deliveryPublishOptions{Target: testDeliveryTarget()}},
		"not hosted": {workspace: workspace, options: deliveryPublishOptions{Target: testDeliveryTarget()}},
	} {
		inventory := hosting
		if name == "not hosted" {
			inventory = &Inventory{Module: "crm"}
		}
		require.NoError(t, publication.declareEnvelope(inventory, "prod"), name)
		if name != "hosted" {
			require.Nil(t, publication.options.Envelope, name)
			continue
		}
		require.NotNil(t, publication.options.Envelope)
		require.True(t, publication.options.Envelope.Stated)
		require.Contains(t, publication.options.Envelope.Principals, "records")
		require.Equal(t, uint64(3), publication.options.Envelope.MinimumRevision)
	}
	other := &deliveryPublication{workspace: workspace, options: deliveryPublishOptions{Target: testDeliveryTarget()}}
	require.NoError(t, other.declareEnvelope(hosting, "staging"))
	require.False(t, other.options.Envelope.Stated, "an environment with no file states nothing")
}

// TestTheMinimumRevisionLiftsTheSettledRevision: a floor above the settled
// revision is the revision delivered; one below leaves the settled revision;
// one that lifts an unchanged envelope is a new revision, signed anew; and an
// unchanged envelope already delivered at the floor keeps its carrier.
func TestTheMinimumRevisionLiftsTheSettledRevision(t *testing.T) {
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{}
	floored := func(principals map[string]json.RawMessage, floor uint64) deliveryPublishOptions {
		opts := envelopeOptions(t, signer, principals)
		opts.Envelope.MinimumRevision = floor
		return opts
	}
	revision := func(delivery *InventoryDelivery) uint64 {
		return documentByID(delivery, deliveredEnvelopeID).Generation
	}
	carrier := func() string {
		return deliveredEnvelope(t, repository).Data[solutionhost.EnvelopeSignedFileName]
	}

	// Above the first revision: the floor.
	opts := floored(recordsGrant("tracing"), 5)
	delivery, err := repository.settle(repository.stageRender(t, ""), &opts)
	require.NoError(t, err)
	require.Equal(t, uint64(5), revision(delivery))
	require.Equal(t, uint64(5), verifyAsTheHost(t, signer, carrier()).Revision)
	repository.deliver(t)

	// Below the settled revision: the settled one (changed content, 5 + 1).
	opts = floored(recordsGrant("annotations"), 2)
	delivery, err = repository.settle(repository.stageRender(t, ""), &opts)
	require.NoError(t, err)
	require.Equal(t, uint64(6), revision(delivery))
	repository.deliver(t)

	// Unchanged content lifted by a floor: a new revision, re-signed.
	signed, before := signer.signed, carrier()
	opts = floored(recordsGrant("annotations"), 9)
	delivery, err = repository.settle(repository.stageRender(t, ""), &opts)
	require.NoError(t, err)
	require.Equal(t, uint64(9), revision(delivery))
	require.Equal(t, signed+1, signer.signed, "a lifted revision is a new signature")
	require.NotEqual(t, before, carrier())
	require.Equal(t, uint64(9), verifyAsTheHost(t, signer, carrier()).Revision)
	repository.deliver(t)

	// Unchanged and already at the floor: kept, nothing signed.
	signed, before = signer.signed, carrier()
	delivery, err = repository.settle(repository.stageRender(t, ""), &opts)
	require.NoError(t, err)
	require.Equal(t, uint64(9), revision(delivery))
	require.Equal(t, signed, signer.signed)
	require.Equal(t, before, carrier())
}
