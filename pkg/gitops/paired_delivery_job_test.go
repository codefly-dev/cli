package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
)

// compositionTarget is a delivery endpoint that declares
// codefly.dev/composition: the host takes both signed halves through
// ApplyManifest at its gRPC address.
func compositionTarget() *DeliveryTarget {
	return &DeliveryTarget{
		URL: "http://accounts.platform.svc.cluster.local:8081", Audience: "accounts",
		Address: "accounts.platform.svc.cluster.local:8081", Composition: true, API: "connect",
	}
}

// pairedOptions are publishOptions toward a composition host, reading every
// module's declaration as one that declares the three roles.
func pairedOptions(signer *fakeSigner) deliveryPublishOptions {
	opts := publishOptions(signer)
	opts.Target = compositionTarget()
	opts.Declarations = &AuthoritySources{Roles: func(context.Context, string) ([]*compositionv1.RoleV1, error) {
		return []*compositionv1.RoleV1{{Name: "installer"}, {Name: "editor"}, {Name: "viewer"}}, nil
	}}
	return opts
}

// overlayFile reads one file of the settled delivery overlay.
func overlayFile(t *testing.T, repository *deliveryRepository, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repository.target, filepath.FromSlash(moduleManifestOverlay("prod")), name))
	require.NoError(t, err)
	return data
}

// deliveredJob reads the delivery Job of one kind from the settled overlay.
func deliveredJob(t *testing.T, repository *deliveryRepository, kind string) deliveryContainer {
	t.Helper()
	var job deliveryJobManifest
	require.NoError(t, yaml.Unmarshal(overlayFile(t, repository, "deliver-"+kind+".yaml"), &job))
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	return job.Spec.Template.Spec.Containers[0]
}

// pairedDocumentsIn reads every paired ConfigMap of the settled overlay, keyed
// by binding.
func pairedDocumentsIn(t *testing.T, repository *deliveryRepository) map[string]*compositionv1.ApplyManifestRequest {
	t.Helper()
	documents := map[string]*compositionv1.ApplyManifestRequest{}
	for name, configMap := range settledDocuments(t, repository) {
		if configMap.Metadata.Labels[documentLabel] != pairedDeliveryKind {
			continue
		}
		require.Equal(t, pairedDocumentConfigMapName(configMap.Metadata.Labels[bindingLabel]), configMap.Metadata.Name)
		require.Equal(t, configMap.Metadata.Labels[bindingLabel]+"-paired", name)
		var request compositionv1.ApplyManifestRequest
		require.NoError(t, protojson.Unmarshal([]byte(configMap.Data[PairedDocumentKey]), &request))
		require.Empty(t, request.GetCredential(), "a rendered document never holds a credential")
		documents[configMap.Metadata.Labels[bindingLabel]] = &request
	}
	return documents
}

// A publish toward a composition host writes one paired document per manifest
// — the manifest's own signed carrier with the authority half beside it, roles
// included — and a Job that runs codefly's paired runner from the paired image,
// with no shell and none of the shell runner's environment. A withdrawal is
// still delivered, Core-only, because a removed module declares nothing.
func TestAPairedPublishWritesAPairedDocumentPerManifestAndTheCodeflyJob(t *testing.T) {
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{}
	opts := pairedOptions(signer)
	_, err := repository.settle(repository.stageRender(t, "billing"), &opts)
	require.NoError(t, err)
	repository.deliver(t)

	_, err = repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	documents := pairedDocumentsIn(t, repository)
	require.Len(t, documents, 2, "one paired document per manifest: the new crm and the billing withdrawal")
	manifests := settledDocuments(t, repository)
	for binding, request := range documents {
		require.Equal(t, []byte(manifests[binding].Data[solutionhost.SignedFileName]), carrierBytes(t, request),
			"the Core half of %s is the manifest's own signed carrier", binding)
	}
	var policy compositionv1.AuthorityManifestV1
	require.NotNil(t, documents["example.prod.crm"].GetAuthority())
	require.NoError(t, proto.Unmarshal(documents["example.prod.crm"].GetAuthority().GetDocument(), &policy))
	require.Len(t, policy.GetRoles(), 3, "the roles reach the host on the authority half")
	require.Nil(t, documents["example.prod.billing"].GetAuthority(), "a withdrawal carries no authority half")
	kustomization := string(overlayFile(t, repository, kustomizationFile))
	for binding := range documents {
		require.Contains(t, kustomization, pairedDocumentFile(binding), "Argo applies what the kustomization names")
	}

	container := deliveredJob(t, repository, deliveryManifest)
	require.Equal(t, pairedDeliveryImage, container.Image)
	require.Equal(t, []string{
		"/bin/codefly", "delivery", "apply",
		"--documents", deliveryDocumentsMount,
		"--address", compositionTarget().Address,
		"--identity-file", deliveryIdentityMount + "/" + deliveryTokenFile,
	}, container.Command[:9])
	require.Empty(t, container.Env, "a paired Job's inputs are its arguments; no DELIVERY_* variable")
	jobBytes := string(overlayFile(t, repository, "deliver-"+deliveryManifest+".yaml"))
	for binding := range documents {
		require.Contains(t, jobBytes, "name: "+pairedDocumentConfigMapName(binding), "the Job mounts the paired ConfigMap")
	}
	require.Contains(t, jobBytes, "key: "+PairedDocumentKey)
	require.NotContains(t, jobBytes, "key: "+solutionhost.SignedFileName, "the Core-only carrier is not what a paired Job sends")
}

// carrierBytes re-encodes a paired document's Core half as the carrier the
// manifest ConfigMap holds, so the two can be compared byte for byte.
func carrierBytes(t *testing.T, request *compositionv1.ApplyManifestRequest) []byte {
	t.Helper()
	signed, err := solutionhost.Carrier(request.GetCarrier().GetDocument(), request.GetCarrier().GetEnvelope())
	require.NoError(t, err)
	carrier, err := solutionhost.MarshalSigned(signed)
	require.NoError(t, err)
	return carrier
}

// A host that declares no composition interface keeps the shell runner: the
// Core-only carrier POSTed over HTTP, from the shell image, and no paired
// document is written.
func TestANonPairedPublishKeepsTheShellRunner(t *testing.T) {
	repository := newDeliveryRepository(t)
	opts := publishOptions(&fakeSigner{})
	_, err := repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	require.Empty(t, pairedDocumentsIn(t, repository))
	container := deliveredJob(t, repository, deliveryManifest)
	require.Equal(t, deliveryImage, container.Image)
	require.Equal(t, []string{"/bin/sh", "-ec", deliveryScript}, container.Command)
	require.Contains(t, container.Env, deliveryEnv{Name: "DELIVERY_PATH", Value: manifestDeliveryPath})
	require.Contains(t, string(overlayFile(t, repository, "deliver-"+deliveryManifest+".yaml")), "key: "+solutionhost.SignedFileName)
}

// A paired publish that cannot read declarations refuses by name rather than
// deliver the Core half alone, which would reach the host with no roles and
// look like a success.
func TestAPairedPublishWithNoDeclarationsIsRefused(t *testing.T) {
	repository := newDeliveryRepository(t)
	opts := pairedOptions(&fakeSigner{})
	opts.Declarations = nil
	_, err := repository.settle(repository.stageRender(t, "crm"), &opts)
	require.ErrorContains(t, err, CompositionInterface)
	require.ErrorContains(t, err, compositionTarget().Address)
	require.ErrorContains(t, err, "will not deliver the Core half alone")
}

// The composition interface has no envelope method, so an envelope Job keeps
// the shell runner toward a composition host — but runs the paired IMAGE,
// because the cell admits one carrier per namespace and the host's namespace
// runs the manifest Job too. Both Jobs, in both modes, run exactly the image
// the cell declares for that namespace.
func TestEveryDeliveryJobRunsTheImageTheCellAdmits(t *testing.T) {
	env := &environments.Environment{Name: "prod", Host: testHost()}
	for _, paired := range []bool{false, true} {
		target := testDeliveryTarget()
		if paired {
			target = compositionTarget()
		}
		declared, err := cellDelivery(env, "crm", paired)
		require.NoError(t, err)
		for kind, path := range map[string]string{deliveryManifest: manifestDeliveryPath, deliveredEnvelopeKind: envelopeDeliveryPath} {
			directory := t.TempDir()
			file, err := renderDeliveryJob(directory, deliveryJobName(kind, "crm", "prod", [][]byte{[]byte("carrier")}), "crm",
				kind, path, target, []deliveryDocument{{ConfigMap: "documents", Key: "document.json", Name: "crm.json"}})
			require.NoError(t, err)
			data, err := os.ReadFile(filepath.Join(directory, file))
			require.NoError(t, err)
			var job deliveryJobManifest
			require.NoError(t, yaml.Unmarshal(data, &job))
			container := job.Spec.Template.Spec.Containers[0]
			require.Equal(t, declared.Image.Repository+"@"+declared.Image.Digest, container.Image,
				"paired=%v %s: the Job runs the image the cell admits", paired, kind)
			require.Equal(t, declared.Selector, job.Spec.Template.Metadata.Labels)
			runsCodefly := strings.HasPrefix(strings.Join(container.Command, " "), "/bin/codefly delivery apply")
			require.Equal(t, paired && kind == deliveryManifest, runsCodefly,
				"paired=%v %s: only a manifest has an ApplyManifest; an envelope is POSTed by the shell runner", paired, kind)
			if !runsCodefly {
				require.Contains(t, container.Env, deliveryEnv{Name: "DELIVERY_PATH", Value: path})
			}
		}
	}
}

// A paired document is signed once: the publish that executes an inspected
// plan delivers the exact pair the plan prepared, because a fresh signature is
// fresh bytes and would make the plan refuse itself as stale. Executing a plan
// that prepared no pair for a binding is refused rather than signed.
func TestAPairedDocumentIsSignedOnceAndCarriedToTheExecutingPublish(t *testing.T) {
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{nonce: true}
	planned := pairedOptions(signer)
	_, err := repository.settle(repository.stageRender(t, "crm"), &planned)
	require.NoError(t, err)
	first := pairedDocumentsIn(t, repository)
	key := carrierKey(pairedDeliveryKind, "example.prod.crm", 1)
	require.NotEmpty(t, planned.Carriers[key], "the plan hands the pair to the publish that executes it")
	signedByPlan := signer.signed

	executing := pairedOptions(signer)
	executing.Reuse = planned.Carriers
	executing.Executing = true
	_, err = repository.settle(repository.stageRender(t, "crm"), &executing)
	require.NoError(t, err)
	second := pairedDocumentsIn(t, repository)
	require.Equal(t, signedByPlan, signer.signed, "execution signs nothing the plan did not")
	require.Equal(t, first["example.prod.crm"].GetAuthority().GetEnvelope(), second["example.prod.crm"].GetAuthority().GetEnvelope(),
		"the executed pair is byte for byte the planned one")

	unplanned := pairedOptions(signer)
	unplanned.Reuse = map[string][]byte{}
	for planned, carrier := range planned.Carriers {
		if planned != key {
			unplanned.Reuse[planned] = carrier // the Core carrier the plan signed, and not the pair
		}
	}
	unplanned.Executing = true
	_, err = repository.settle(repository.stageRender(t, "crm"), &unplanned)
	require.ErrorContains(t, err, "was not prepared by the inspected plan")
}

// A manifest the base branch delivered Core-only enters paired settlement at
// the next generation even when its content is unchanged: the host holds that
// generation with no authority document and refuses the same generation
// offered with one as a history conflict, so the pair is signed at a new one.
// Once delivered paired, an unchanged manifest keeps its generation.
func TestAnUnchangedManifestEnteringPairedSettlementAdvancesOnce(t *testing.T) {
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{}
	coreOnly := publishOptions(signer)
	_, err := repository.settle(repository.stageRender(t, "crm"), &coreOnly)
	require.NoError(t, err)
	require.Equal(t, uint64(1), settledGenerationOf(t, repository, "example.prod.crm"))
	repository.deliver(t)

	paired := pairedOptions(signer)
	_, err = repository.settle(repository.stageRender(t, "crm"), &paired)
	require.NoError(t, err)
	require.Equal(t, uint64(2), settledGenerationOf(t, repository, "example.prod.crm"), "entering paired settlement advances once")
	require.NotNil(t, pairedDocumentsIn(t, repository)["example.prod.crm"].GetAuthority())
	repository.deliver(t)

	again := pairedOptions(signer)
	_, err = repository.settle(repository.stageRender(t, "crm"), &again)
	require.NoError(t, err)
	require.Equal(t, uint64(2), settledGenerationOf(t, repository, "example.prod.crm"), "delivered paired, unchanged content keeps its generation")
}

// Toward a composition host the generation's content is the pair, as the
// native run's record keys it: a module whose declaration changes while its
// manifest does not — an operation gained, a role added — is signed at the
// next generation, because the host holds one immutable pair per generation
// and refuses the same number offered with other authority bytes as a
// history conflict. The declaration unchanged, the generation is kept; the
// authority half vanishing is a change like any other.
func TestAChangedDeclarationAtAnUnchangedManifestAdvancesTheGeneration(t *testing.T) {
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{}
	declaring := func(roles ...string) deliveryPublishOptions {
		opts := pairedOptions(signer)
		opts.Declarations = &AuthoritySources{Roles: func(context.Context, string) ([]*compositionv1.RoleV1, error) {
			declared := make([]*compositionv1.RoleV1, 0, len(roles))
			for _, role := range roles {
				declared = append(declared, &compositionv1.RoleV1{Name: role})
			}
			return declared, nil
		}}
		return opts
	}
	first := declaring("installer", "editor", "viewer")
	_, err := repository.settle(repository.stageRender(t, "crm"), &first)
	require.NoError(t, err)
	require.Equal(t, uint64(1), settledGenerationOf(t, repository, "example.prod.crm"))
	repository.deliver(t)

	grown := declaring("installer", "editor", "viewer", "auditor")
	_, err = repository.settle(repository.stageRender(t, "crm"), &grown)
	require.NoError(t, err)
	require.Equal(t, uint64(2), settledGenerationOf(t, repository, "example.prod.crm"), "a declaration change at an unchanged manifest is the next generation")
	pair := pairedDocumentsIn(t, repository)["example.prod.crm"]
	var policy compositionv1.AuthorityManifestV1
	require.NoError(t, proto.Unmarshal(pair.GetAuthority().GetDocument(), &policy))
	require.Equal(t, uint64(2), policy.GetGeneration(), "the authority half is signed at the new generation")
	require.Len(t, policy.GetRoles(), 4)
	repository.deliver(t)

	same := declaring("installer", "editor", "viewer", "auditor")
	_, err = repository.settle(repository.stageRender(t, "crm"), &same)
	require.NoError(t, err)
	require.Equal(t, uint64(2), settledGenerationOf(t, repository, "example.prod.crm"), "the same pair keeps its generation")
	repository.deliver(t)

	silent := declaring()
	_, err = repository.settle(repository.stageRender(t, "crm"), &silent)
	require.NoError(t, err)
	require.Equal(t, uint64(3), settledGenerationOf(t, repository, "example.prod.crm"), "the authority half vanishing is a change")
	require.Nil(t, pairedDocumentsIn(t, repository)["example.prod.crm"].GetAuthority())
}

// A tree from before paired settlement advanced on entering can pair a
// binding at the very generation it had delivered Core-only, and whether the
// host took that generation Core-only first is written nowhere in the tree:
// settlement keeps the pair, and the host's refusal is what says the number
// is spent. --advance is the deliberate answer: the named binding is signed at
// the next generation although unchanged, and keeps it after; a name the
// base branch delivers nothing under is refused rather than ignored.
func TestANamedBindingAdvancesAlthoughUnchanged(t *testing.T) {
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{}
	coreOnly := publishOptions(signer)
	_, err := repository.settle(repository.stageRender(t, "crm"), &coreOnly)
	require.NoError(t, err)
	require.Equal(t, uint64(1), settledGenerationOf(t, repository, "example.prod.crm"))
	repository.deliver(t)

	// The pre-settlement publish: the pair written beside the delivered
	// carrier at its own generation, and promoted as such.
	paired := pairedOptions(signer)
	carrier := settledDocuments(t, repository)["example.prod.crm"]
	declaration, err := paired.Declarations.Declaration(context.Background(), "crm")
	require.NoError(t, err)
	pair, err := PreparedPairedDocument([]byte(carrier.Data[solutionhost.SignedFileName]), declaration, signer)
	require.NoError(t, err)
	directory := filepath.Join(repository.target, filepath.FromSlash(moduleManifestOverlay("prod")))
	tree, err := newRenderRoot(directory)
	require.NoError(t, err)
	require.NoError(t, writePairedDocumentInTree(tree, directory, carrier.Metadata.Namespace, "example.prod.crm", pair))
	require.NoError(t, tree.Close())
	repository.deliver(t)

	_, err = repository.settle(repository.stageRender(t, "crm"), &paired)
	require.NoError(t, err)
	require.Equal(t, uint64(1), settledGenerationOf(t, repository, "example.prod.crm"), "the tree cannot tell a spent generation from a taken one; unnamed, the pair is kept")

	mistyped := pairedOptions(signer)
	mistyped.Advance = []string{"example.prod.crn"}
	_, err = repository.settle(repository.stageRender(t, "crm"), &mistyped)
	require.ErrorContains(t, err, "--advance names binding example.prod.crn, which the base branch delivers no manifest of")

	advanced := pairedOptions(signer)
	advanced.Advance = []string{"example.prod.crm"}
	_, err = repository.settle(repository.stageRender(t, "crm"), &advanced)
	require.NoError(t, err)
	require.Equal(t, uint64(2), settledGenerationOf(t, repository, "example.prod.crm"), "named, the binding is offered at the next generation")
	require.NotNil(t, pairedDocumentsIn(t, repository)["example.prod.crm"].GetAuthority())
	repository.deliver(t)

	again := pairedOptions(signer)
	_, err = repository.settle(repository.stageRender(t, "crm"), &again)
	require.NoError(t, err)
	require.Equal(t, uint64(2), settledGenerationOf(t, repository, "example.prod.crm"), "delivered paired at a clean generation, unchanged content keeps it")
}

// settledGenerationOf reads the generation the settled overlay carries for a
// binding.
func settledGenerationOf(t *testing.T, repository *deliveryRepository, binding string) uint64 {
	t.Helper()
	document := settledDocuments(t, repository)[binding]
	encoded, err := yaml.Marshal(document)
	require.NoError(t, err)
	manifest, _, held, err := manifestFromConfigMap(encoded)
	require.NoError(t, err)
	require.True(t, held)
	return manifest.Generation
}

// An unchanged pair is delivered again as the bytes the base branch carries,
// not signed again: a signature is fresh bytes each time (ECDSA, KMS), so a
// pair signed anew changed the overlay, the delivery Job's name and the
// promotion on every publish of an unchanged render, and a whole-composition
// deploy run again found no module already delivered (2026-11-13, w9scratch:
// a resumed deploy re-promoted all thirteen modules). A changed declaration
// is still signed, at the next generation.
func TestAnUnchangedPairIsDeliveredAsTheBaseCarriesIt(t *testing.T) {
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{nonce: true}
	first := pairedOptions(signer)
	_, err := repository.settle(repository.stageRender(t, "crm"), &first)
	require.NoError(t, err)
	delivered := overlayFile(t, repository, pairedDocumentFile("example.prod.crm"))
	repository.deliver(t)
	signed := signer.signed

	again := pairedOptions(signer)
	_, err = repository.settle(repository.stageRender(t, "crm"), &again)
	require.NoError(t, err)
	require.Equal(t, string(delivered), string(overlayFile(t, repository, pairedDocumentFile("example.prod.crm"))),
		"an unchanged pair is the delivered one, byte for byte")
	require.Equal(t, signed, signer.signed, "nothing was signed again")

	// A pair its signer no longer verifies (another key) is signed again,
	// never delivered on trust.
	rotated := &fakeSigner{nonce: true}
	other := pairedOptions(rotated)
	_, err = repository.settle(repository.stageRender(t, "crm"), &other)
	require.NoError(t, err)
	require.Positive(t, rotated.signed, "a pair the signer cannot verify is signed again")

	grown := pairedOptions(signer)
	grown.Declarations = &AuthoritySources{Roles: func(context.Context, string) ([]*compositionv1.RoleV1, error) {
		return []*compositionv1.RoleV1{{Name: "installer"}, {Name: "editor"}, {Name: "viewer"}, {Name: "auditor"}}, nil
	}}
	_, err = repository.settle(repository.stageRender(t, "crm"), &grown)
	require.NoError(t, err)
	require.NotEqual(t, string(delivered), string(overlayFile(t, repository, pairedDocumentFile("example.prod.crm"))))
	require.Equal(t, uint64(2), settledGenerationOf(t, repository, "example.prod.crm"))
}
