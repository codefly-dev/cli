package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	delivery "github.com/codefly-dev/authority/libraries/delivery/go"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
)

// signedCarrierOf is the Core half exactly as it is delivered: the settled
// manifest, signed, in the carrier the host reads.
func signedCarrierOf(t *testing.T, manifest *solutionhost.ModuleManifest, signer solutionhost.Signer) []byte {
	t.Helper()
	payload, err := solutionhost.SignedPayload(manifest)
	require.NoError(t, err)
	envelope, err := solutionhost.SignEnvelope(payload, signer)
	require.NoError(t, err)
	signed, err := solutionhost.Carrier(payload, envelope)
	require.NoError(t, err)
	carrier, err := solutionhost.MarshalSigned(signed)
	require.NoError(t, err)
	return carrier
}

func preparedPairManifest(binding string, generation uint64) *solutionhost.ModuleManifest {
	return &solutionhost.ModuleManifest{
		Schema: solutionhost.SchemaModuleManifestV1, Binding: binding, Alias: binding, Kind: solutionhost.KindModule, Generation: generation,
		Host:      solutionhost.HostTarget{Coordinate: "local", Domain: "local"},
		Release:   solutionhost.Release{Publisher: "test", Name: binding, Version: "0.1.0"},
		Workloads: []solutionhost.Workload{{Service: "api", Container: "app"}},
		Authority: &solutionhost.Authority{Principal: binding, InstallationRole: "installer"},
	}
}

// The prepared document carries both halves, and the authority half is stamped
// with the module, the generation and the digest of the EXACT Core bytes
// beside it — so a signed authority cannot be replayed against another
// manifest, generation or module.
func TestAPreparedDocumentBindsTheAuthorityHalfToTheCoreBytes(t *testing.T) {
	signer := nativeSigner(t)
	manifest := preparedPairManifest("wiki", 7)
	carrier := signedCarrierOf(t, manifest, signer)
	declaration, err := proto.Marshal(&compositionv1.AuthorityManifestV1{
		InstallationRole: "installer",
		Roles:            []*compositionv1.RoleV1{{Name: "installer"}, {Name: "editor"}, {Name: "viewer"}},
	})
	require.NoError(t, err)

	data, err := PreparedPairedDocument(carrier, declaration, signer)
	require.NoError(t, err)

	var request compositionv1.ApplyManifestRequest
	require.NoError(t, protojson.Unmarshal(data, &request))
	require.Empty(t, request.GetCredential(), "a rendered document never holds a credential")
	require.NotNil(t, request.GetCarrier())
	require.NotNil(t, request.GetAuthority(), "the half the legacy HTTP route cannot carry")

	var policy compositionv1.AuthorityManifestV1
	require.NoError(t, proto.Unmarshal(request.GetAuthority().GetDocument(), &policy))
	require.Equal(t, "wiki", policy.GetModule())
	require.Equal(t, uint64(7), policy.GetGeneration())
	require.Equal(t, delivery.Digest(request.GetCarrier().GetDocument()), policy.GetCoreManifestDigest(),
		"bound to the exact Core bytes beside it")
	require.Equal(t, []string{"installer", "editor", "viewer"},
		[]string{policy.GetRoles()[0].GetName(), policy.GetRoles()[1].GetName(), policy.GetRoles()[2].GetName()},
		"the roles a module declares travel here and nowhere else: core's Authority has no roles field")
	require.Equal(t, "installer", policy.GetInstallationRole())
}

// The two producers of the authority half — a native run and a publish
// preparing a document for a cell to send — must agree byte for byte, or the
// host compares one rule against two implementations of it.
func TestBothProducersOfTheAuthorityHalfAgreeByteForByte(t *testing.T) {
	signer := nativeSigner(t)
	manifest := preparedPairManifest("wiki", 3)
	carrier := signedCarrierOf(t, manifest, signer)
	declaration, err := proto.Marshal(&compositionv1.AuthorityManifestV1{
		InstallationRole: "installer",
		EdgeDependencies: []*compositionv1.EdgeDependencyV1{{Module: "accounts"}, {Module: "documents"}},
	})
	require.NoError(t, err)
	signed, err := solutionhost.ParseSigned(carrier)
	require.NoError(t, err)

	// The native producer, through the one it has always used.
	native := &CompositionDelivery{signer: signer}
	fromRun, err := native.authority(manifest, signed.Document, declaration)
	require.NoError(t, err)

	// The publish producer, through the prepared document.
	data, err := PreparedPairedDocument(carrier, declaration, signer)
	require.NoError(t, err)
	var request compositionv1.ApplyManifestRequest
	require.NoError(t, protojson.Unmarshal(data, &request))

	require.Equal(t, fromRun.GetDocument(), request.GetAuthority().GetDocument(),
		"the same declaration, the same stamp, the same deterministic bytes")
	require.Equal(t, fromRun.GetEnvelope(), request.GetAuthority().GetEnvelope(),
		"and therefore the same signature")
}

// A withdrawal keeps its Core-only identity: it declares nothing, so it
// carries no authority half, and the sender still delivers it.
func TestAWithdrawalCarriesNoAuthorityHalfAndIsStillDelivered(t *testing.T) {
	signer := nativeSigner(t)
	manifest := preparedPairManifest("wiki", 9)
	manifest.Removed = true
	manifest.Workloads = nil
	manifest.Authority = nil
	carrier := signedCarrierOf(t, manifest, signer)

	data, err := PreparedPairedDocument(carrier, nil, signer)
	require.NoError(t, err)
	var request compositionv1.ApplyManifestRequest
	require.NoError(t, protojson.Unmarshal(data, &request))
	require.Nil(t, request.GetAuthority(), "a withdrawal declares nothing")
	require.NotNil(t, request.GetCarrier())

	directory := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(directory, "withdrawal.json"), data, 0o600))
	host := &applyHost{}
	sender := &PairedApply{Client: applyHostClient(t, host), Credential: func() (string, error) { return "t", nil }, Sleep: noPause}
	receipts, err := sender.Apply(context.Background(), directory)
	require.NoError(t, err)
	require.True(t, receipts[0].Accepted)
	require.Nil(t, host.seen[0].GetAuthority())
}

// The bytes are the Job's name, so an unchanged pair must encode identically —
// across processes, since protojson's whitespace seed is per-binary.
func TestAnUnchangedPairEncodesIdentically(t *testing.T) {
	signer := nativeSigner(t)
	carrier := signedCarrierOf(t, preparedPairManifest("wiki", 2), signer)
	declaration, err := proto.Marshal(&compositionv1.AuthorityManifestV1{InstallationRole: "none"})
	require.NoError(t, err)
	first, err := PreparedPairedDocument(carrier, declaration, signer)
	require.NoError(t, err)
	for range 5 {
		again, err := PreparedPairedDocument(carrier, declaration, signer)
		require.NoError(t, err)
		require.Equal(t, string(first), string(again))
	}
	require.NotContains(t, string(first), "  ", "compacted: no insignificant whitespace to perturb")
}

// Without the release key there is no authority half to sign, and the producer
// says so rather than preparing a Core-only document that would reach the host
// with no roles and look like a success.
func TestAPairedDocumentIsNotPreparedWithoutTheReleaseKey(t *testing.T) {
	carrier := signedCarrierOf(t, preparedPairManifest("wiki", 2), nativeSigner(t))
	declaration, err := proto.Marshal(&compositionv1.AuthorityManifestV1{InstallationRole: "installer"})
	require.NoError(t, err)
	_, err = PreparedPairedDocument(carrier, declaration, nil)
	require.ErrorContains(t, err, "signed by the release key")
}

// writeOverlayCarrier writes one module manifest ConfigMap into a delivery
// overlay the way a render does, so prepare reads what a render leaves.
func writeOverlayCarrier(t *testing.T, overlay string, manifest *solutionhost.ModuleManifest, carrier []byte) {
	t.Helper()
	tree, err := newRenderRoot(overlay)
	require.NoError(t, err)
	defer tree.Close()
	_, err = writeModuleManifestInTree(tree, overlay, "acme", manifest, carrier)
	require.NoError(t, err)
}

// Prepare reads the carriers a render settled and the declarations the modules
// hold, and writes one paired document per manifest — in binding order, so the
// Job's digest of its document set is stable.
func TestPrepareWritesAPairedDocumentPerSettledCarrier(t *testing.T) {
	signer := nativeSigner(t)
	overlay := t.TempDir()
	for _, binding := range []string{"wiki", "accounts"} {
		manifest := preparedPairManifest(binding, 2)
		writeOverlayCarrier(t, overlay, manifest, signedCarrierOf(t, manifest, signer))
	}
	sources := AuthoritySources{
		Roles: func(_ context.Context, module string) ([]*compositionv1.RoleV1, error) {
			if module != "wiki" {
				return nil, nil
			}
			return []*compositionv1.RoleV1{{Name: "installer"}, {Name: "editor"}, {Name: "viewer"}}, nil
		},
		Installation: func(_ context.Context, module string) (string, []*compositionv1.EdgeDependencyV1, error) {
			if module != "wiki" {
				return "", nil, nil
			}
			return "installer", nil, nil
		},
	}
	out := t.TempDir()
	written, err := PreparePairedDocuments(context.Background(), overlay, out, sources, signer)
	require.NoError(t, err)
	require.Equal(t, []string{"accounts.json", "wiki.json"}, written, "binding order, so the document set's digest is stable")

	wiki := readPrepared(t, out, "wiki.json")
	require.NotNil(t, wiki.GetAuthority())
	var policy compositionv1.AuthorityManifestV1
	require.NoError(t, proto.Unmarshal(wiki.GetAuthority().GetDocument(), &policy))
	require.Equal(t, []string{"installer", "editor", "viewer"},
		[]string{policy.GetRoles()[0].GetName(), policy.GetRoles()[1].GetName(), policy.GetRoles()[2].GetName()})
	require.Equal(t, "installer", policy.GetInstallationRole())

	accounts := readPrepared(t, out, "accounts.json")
	require.Nil(t, accounts.GetAuthority(), "a module that declares nothing carries no authority half")
	require.NotNil(t, accounts.GetCarrier())
}

// An unsigned carrier cannot carry a signed authority half beside it. Refusing
// by name is the whole point: a Core-only document would reach the host looking
// like a success and compose the module with no roles — the defect this path
// exists to end.
func TestPrepareRefusesAnUnsignedCarrierRatherThanDropTheAuthorityHalf(t *testing.T) {
	overlay := t.TempDir()
	manifest := preparedPairManifest("wiki", 2)
	writeOverlayCarrier(t, overlay, manifest, nil)
	_, err := PreparePairedDocuments(context.Background(), overlay, t.TempDir(),
		AuthoritySources{Roles: func(context.Context, string) ([]*compositionv1.RoleV1, error) {
			return []*compositionv1.RoleV1{{Name: "installer"}}, nil
		}}, nativeSigner(t))
	require.ErrorContains(t, err, "carries no signature")
	require.ErrorContains(t, err, "wiki")
}

// An overlay with no manifest is not an empty success.
func TestPrepareRefusesAnOverlayWithNoManifest(t *testing.T) {
	_, err := PreparePairedDocuments(context.Background(), t.TempDir(), t.TempDir(), AuthoritySources{}, nativeSigner(t))
	require.ErrorContains(t, err, "nothing to deliver")
}

// What prepare writes is what apply sends: the two halves survive the file and
// reach a real host over the generated client.
func TestWhatPrepareWritesIsWhatApplySends(t *testing.T) {
	signer := nativeSigner(t)
	overlay := t.TempDir()
	manifest := preparedPairManifest("wiki", 5)
	writeOverlayCarrier(t, overlay, manifest, signedCarrierOf(t, manifest, signer))
	out := t.TempDir()
	_, err := PreparePairedDocuments(context.Background(), overlay, out, AuthoritySources{
		Installation: func(context.Context, string) (string, []*compositionv1.EdgeDependencyV1, error) {
			return "installer", []*compositionv1.EdgeDependencyV1{{Module: "accounts"}}, nil
		},
	}, signer)
	require.NoError(t, err)

	host := &applyHost{}
	sender := &PairedApply{Client: applyHostClient(t, host), Credential: func() (string, error) { return "t", nil }, Sleep: noPause}
	receipts, err := sender.Apply(context.Background(), out)
	require.NoError(t, err)
	require.True(t, receipts[0].Accepted)
	require.Equal(t, uint64(5), receipts[0].Generation, "the generation the render settled, read from the signed document")
	var policy compositionv1.AuthorityManifestV1
	require.NoError(t, proto.Unmarshal(host.seen[0].GetAuthority().GetDocument(), &policy))
	require.Equal(t, "installer", policy.GetInstallationRole())
	require.Equal(t, []string{"accounts"}, []string{policy.GetEdgeDependencies()[0].GetModule()})
	require.Equal(t, delivery.Digest(host.seen[0].GetCarrier().GetDocument()), policy.GetCoreManifestDigest())
}

func readPrepared(t *testing.T, directory, name string) *compositionv1.ApplyManifestRequest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, name))
	require.NoError(t, err)
	var request compositionv1.ApplyManifestRequest
	require.NoError(t, protojson.Unmarshal(data, &request))
	require.Empty(t, request.GetCredential(), "a rendered document never holds a credential")
	return &request
}
