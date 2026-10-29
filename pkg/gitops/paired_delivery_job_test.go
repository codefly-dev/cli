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
