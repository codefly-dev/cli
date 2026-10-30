package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	"github.com/codefly-dev/authority/libraries/authority-core/go/delivered"
	"github.com/codefly-dev/authority/libraries/authority-core/go/domain"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// documentOf is the host's proto -> delivered.Document mapping, for the two
// declaration fields this proof is about. The mapping itself belongs to the
// Authority service, not to its shared library, so it is reproduced here and
// is NOT what this test proves: what it proves is that the two carriers the
// CLI produces AGREE under Authority's real sameInstallationDeclaration rule.
func documentOf(t *testing.T, sidecar []byte, manifest *solutionhost.ModuleManifest) delivered.Document {
	t.Helper()
	var policy compositionv1.AuthorityManifestV1
	require.NoError(t, proto.Unmarshal(sidecar, &policy))
	doc := delivered.Document{
		Module:           manifest.Alias,
		Generation:       manifest.Generation,
		InstallationRole: policy.GetInstallationRole(),
	}
	for _, edge := range policy.GetEdgeDependencies() {
		doc.EdgeDependencies = append(doc.EdgeDependencies,
			domain.EdgeDependency{Module: edge.GetModule(), Presence: edge.GetPresence()})
	}
	return doc
}

// producedCarriers renders a module declaring `declaration` and returns the two
// signed carriers' declaration content: the Core manifest's Authority block and
// the AuthorityManifestV1 bytes, each from the real producer.
func producedCarriers(t *testing.T, declaration string) (*solutionhost.ModuleManifest, []byte) {
	t.Helper()
	workspace, module := writeAuthorityWorkspaceDeclaring(t, declaration)
	env := selectedEnvironment(t, workspace, "staging")
	_, manifest := renderShopManifest(t, workspace, module, env)

	// The same reader the delivery uses, over the same contract.
	role, edges, err := InstallationDeclaration(module.Dir())
	require.NoError(t, err)
	sidecar, err := proto.MarshalOptions{Deterministic: true}.Marshal(&compositionv1.AuthorityManifestV1{
		InstallationRole: role, EdgeDependencies: edges,
	})
	require.NoError(t, err)
	return manifest, sidecar
}

// TestTheSignedSidecarOnTheWireCarriesTheDeclaration is the actual-wire
// regression the independent review asked for: it drives the REAL
// CompositionDelivery with a REAL contract read, delivers through
// NativeDeliverer, and reads the declaration back off the signed authority
// document the peer received.
//
// The agreement proof below marshals the proto itself, so it cannot catch
// three regressions the review named: dropping the installation callback at
// the construction site, omitting either field in
// CompositionDelivery.declaration, or reverting its declaration-presence
// condition. This does, because the only thing it reads is what went over the
// wire.
func TestTheSignedSidecarOnTheWireCarriesTheDeclaration(t *testing.T) {
	for name, tc := range map[string]struct {
		declaration string
		wantRole    string
		wantEdges   [][2]string
		wantSidecar bool
	}{
		"a named selector with ordered edges": {`installation_role: installer
edge_dependencies:
  - module: robin
    presence: required
  - module: annotations
    presence: optional
`, "installer", [][2]string{{"robin", "required"}, {"annotations", "optional"}}, true},
		"the none selector alone is reason enough for a sidecar": {
			"installation_role: none\n", "none", nil, true},
		"edges alone are reason enough for a sidecar": {`edge_dependencies:
  - module: robin
    presence: required
`, "", [][2]string{{"robin", "required"}}, true},
		"no declaration, and nothing else to declare, means no sidecar": {"", "", nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			// A real contract on disk, read by the real reader.
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, modulecontract.FileName),
				[]byte(declaringContract(tc.declaration)), 0o600))

			conn, peer := pairedConnection(t)
			signer := nativeSigner(t)
			composition := NewCompositionDelivery(conn, signer,
				func(context.Context, string) ([]*compositionv1.MethodPolicyV1, bool, error) { return nil, false, nil },
				func(context.Context, string) ([]*authorityv1.WorkScopeV1, error) { return nil, nil },
				func(context.Context, string) ([]*compositionv1.RoleV1, error) { return nil, nil },
				func(context.Context, string) (bool, error) { return false, nil },
				func(_ context.Context, _ string) (string, []*compositionv1.EdgeDependencyV1, error) {
					return InstallationDeclaration(dir)
				})
			d := &NativeDeliverer{Signer: signer, StateDir: t.TempDir(), Prepare: composition.Prepare,
				Token: func(string) (string, error) { return "credential", nil }}
			_, err := d.Deliver(t.Context(), pairedManifest())
			require.NoError(t, err)

			requests := peer.requests()
			require.Len(t, requests, 1)
			if !tc.wantSidecar {
				require.Nil(t, requests[0].Authority,
					"a module declaring nothing must deliver no authority document")
				return
			}
			require.NotNil(t, requests[0].Authority, "the declaration must produce a signed sidecar")

			var policy compositionv1.AuthorityManifestV1
			require.NoError(t, proto.Unmarshal(requests[0].Authority.GetDocument(), &policy))
			require.Equal(t, tc.wantRole, policy.GetInstallationRole())
			require.Len(t, policy.GetEdgeDependencies(), len(tc.wantEdges))
			for i, want := range tc.wantEdges {
				require.Equal(t, want[0], policy.GetEdgeDependencies()[i].GetModule(),
					"edge %d module, in authored order", i)
				require.Equal(t, want[1], policy.GetEdgeDependencies()[i].GetPresence(), "edge %d presence", i)
			}
		})
	}
}

// TestAnInstallationOnlyChangeAdvancesTheGeneration is the case the existing
// generation test does not cover: TestNativeGenerationIncludesEachAuthorityPolicyInput
// enumerates method, ceiling, role and owner-aware, and passes nil for
// installation.
//
// It matters beyond coverage. A host that already holds a manifest at
// generation 1 with a NULL authority document will not replace it unless a
// later generation arrives — so if editing only the installation declaration
// left the generation where it was, a repaired delivery would re-send
// generation 1 and the host would keep the policy-less record it already has.
func TestAnInstallationOnlyChangeAdvancesTheGeneration(t *testing.T) {
	dir := t.TempDir()
	write := func(declaration string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, modulecontract.FileName),
			[]byte(declaringContract(declaration)), 0o600))
	}
	write("")

	conn, peer := pairedConnection(t)
	signer := nativeSigner(t)
	composition := NewCompositionDelivery(conn, signer,
		func(context.Context, string) ([]*compositionv1.MethodPolicyV1, bool, error) { return nil, false, nil },
		func(context.Context, string) ([]*authorityv1.WorkScopeV1, error) { return nil, nil },
		func(context.Context, string) ([]*compositionv1.RoleV1, error) { return nil, nil },
		func(context.Context, string) (bool, error) { return false, nil },
		func(_ context.Context, _ string) (string, []*compositionv1.EdgeDependencyV1, error) {
			return InstallationDeclaration(dir)
		})
	d := &NativeDeliverer{Signer: signer, StateDir: t.TempDir(), Prepare: composition.Prepare,
		Token: func(string) (string, error) { return "credential", nil }}

	// Declaring nothing: delivered, and no authority document at all — which
	// is how a host ends up holding a record with a NULL document.
	_, err := d.Deliver(t.Context(), pairedManifest())
	require.NoError(t, err)
	require.Nil(t, peer.requests()[0].Authority)

	// Add ONLY the selector. The generation must move, or the host keeps the
	// policy-less record.
	write("installation_role: none\n")
	_, err = d.Deliver(t.Context(), pairedManifest())
	require.NoError(t, err)

	// Add ONLY an edge. The generation must move again.
	write(`installation_role: none
edge_dependencies:
  - module: robin
    presence: required
`)
	_, err = d.Deliver(t.Context(), pairedManifest())
	require.NoError(t, err)

	var generations []uint64
	var documents []bool
	for _, request := range peer.requests() {
		manifest, err := solutionhost.ManifestFromVerified(request.Carrier.Document)
		require.NoError(t, err)
		generations = append(generations, manifest.Generation)
		documents = append(documents, request.Authority != nil)
	}
	require.Equal(t, []uint64{1, 2, 3}, generations,
		"an installation-only edit must advance the generation, or a host holding an older record never replaces it")
	require.Equal(t, []bool{false, true, true}, documents,
		"a declaration is reason enough for a signed authority document to exist")
}

// TestTheProducedCarriersAgreeUnderAuthoritysRule runs the CLI's OWN two
// carriers through Authority's real paired-agreement rule
// (delivered.Pair/Module at authority-core 21ce6d92), rather than asserting the
// CLI's own expectations back at itself.
//
// Authority's own tests begin at a constructed document; this begins at what
// the producer actually emits, which is the gap their suite names and leaves
// open.
func TestTheProducedCarriersAgreeUnderAuthoritysRule(t *testing.T) {
	for name, declaration := range map[string]string{
		"a named selector with ordered edges": `installation_role: installer
edge_dependencies:
  - module: robin
    presence: required
  - module: annotations
    presence: optional
`,
		"the none selector": "installation_role: none\n",
		"no declaration":    "",
		"edges without a selector": `edge_dependencies:
  - module: robin
    presence: required
`,
	} {
		t.Run(name, func(t *testing.T) {
			manifest, sidecar := producedCarriers(t, declaration)
			doc := documentOf(t, sidecar, manifest)

			// The agreement rule, reached through Module, which is what the
			// reader composes a delivery with.
			composed, err := delivered.Module(manifest, &doc)
			require.NoError(t, err, "the CLI's two carriers disagree under Authority's rule")

			// And the declaration survives into what Authority composes.
			require.Equal(t, manifest.Authority.InstallationRole, composed.InstallationRole)
			require.Len(t, composed.EdgeDependencies, len(manifest.Authority.EdgeDependencies))
			for i, edge := range manifest.Authority.EdgeDependencies {
				require.Equal(t, edge.Module, composed.EdgeDependencies[i].Module)
				require.Equal(t, edge.Presence, composed.EdgeDependencies[i].Presence)
			}
		})
	}
}

// TestAuthorityRefusesEveryDisagreementTheRuleNames mutates ONE carrier of a
// genuinely produced pair and asserts Authority refuses. Each case starts from
// a pair that agrees, so the refusal is the disagreement and nothing else.
func TestAuthorityRefusesEveryDisagreementTheRuleNames(t *testing.T) {
	ordered := `installation_role: installer
edge_dependencies:
  - module: robin
    presence: required
  - module: annotations
    presence: optional
`
	for name, tc := range map[string]struct {
		declaration string
		breakDoc    func(*delivered.Document)
		want        string
	}{
		"selector mismatch": {ordered, func(d *delivered.Document) {
			d.InstallationRole = "viewer"
		}, "installation_role differs"},
		"selector one-sided: document declares nothing": {ordered, func(d *delivered.Document) {
			d.InstallationRole = ""
		}, "installation_role differs"},
		"empty is not none": {"installation_role: none\n", func(d *delivered.Document) {
			d.InstallationRole = ""
		}, "installation_role differs"},
		"edges one-sided: document carries none": {ordered, func(d *delivered.Document) {
			d.EdgeDependencies = nil
		}, "edge_dependencies differs"},
		"edge identity mismatch": {ordered, func(d *delivered.Document) {
			d.EdgeDependencies[0].Module = "documents"
		}, "edge_dependencies differs"},
		"edge presence mismatch": {ordered, func(d *delivered.Document) {
			d.EdgeDependencies[1].Presence = "required"
		}, "edge_dependencies differs"},
		"edge order mismatch": {ordered, func(d *delivered.Document) {
			d.EdgeDependencies[0], d.EdgeDependencies[1] = d.EdgeDependencies[1], d.EdgeDependencies[0]
		}, "edge_dependencies differs"},
		"edge count mismatch": {ordered, func(d *delivered.Document) {
			d.EdgeDependencies = append(d.EdgeDependencies,
				domain.EdgeDependency{Module: "tracing", Presence: "optional"})
		}, "edge_dependencies differs"},
	} {
		t.Run(name, func(t *testing.T) {
			manifest, sidecar := producedCarriers(t, tc.declaration)
			doc := documentOf(t, sidecar, manifest)

			// It agrees before the mutation, so the case is not vacuous.
			_, err := delivered.Module(manifest, &doc)
			require.NoError(t, err, "the produced pair must agree before it is broken")

			tc.breakDoc(&doc)
			_, err = delivered.Module(manifest, &doc)
			require.Error(t, err, "Authority admitted a disagreeing pair")
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// A declared nonempty selector with NO sidecar at all refuses: an absent
// document is the one-sided case the rule names explicitly.
func TestAnAbsentDocumentRefusesADeclaredSelector(t *testing.T) {
	manifest, _ := producedCarriers(t, "installation_role: none\n")
	_, err := delivered.Module(manifest, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "installation_role differs")

	// And a module declaring nothing pairs with no document, unchanged — the
	// legacy both-absent case the rule preserves.
	plain, _ := producedCarriers(t, "")
	_, err = delivered.Module(plain, nil)
	require.NoError(t, err)
}

// nil and [] are equivalent zero edges on BOTH carriers: the producer delivers
// nil for an empty list precisely so the two spellings cannot disagree.
func TestNilAndEmptyEdgesAgree(t *testing.T) {
	manifest, sidecar := producedCarriers(t, "installation_role: none\n")
	require.Empty(t, manifest.Authority.EdgeDependencies)

	doc := documentOf(t, sidecar, manifest)
	require.Empty(t, doc.EdgeDependencies)

	// The produced manifest carries nil; an explicitly empty slice on either
	// side is the same zero-edge declaration.
	doc.EdgeDependencies = []domain.EdgeDependency{}
	_, err := delivered.Module(manifest, &doc)
	require.NoError(t, err, "[] and nil must be equivalent zero edges")

	manifest.Authority.EdgeDependencies = []modulecontract.EdgeDependency{}
	_, err = delivered.Module(manifest, &doc)
	require.NoError(t, err)

	_ = context.Background()
}

func TestResolvedSetupAndOwnerKindsSurviveSignedDelivery(t *testing.T) {
	conn, peer := pairedConnection(t)
	signer := nativeSigner(t)
	composition := NewCompositionDelivery(conn, signer,
		func(context.Context, string) ([]*compositionv1.MethodPolicyV1, bool, error) { return nil, false, nil },
		func(context.Context, string) ([]*authorityv1.WorkScopeV1, error) { return nil, nil },
		func(context.Context, string) ([]*compositionv1.RoleV1, error) { return nil, nil },
		func(context.Context, string) (bool, error) { return false, nil }, nil)
	d := &NativeDeliverer{Signer: signer, StateDir: t.TempDir(), Prepare: composition.Prepare, Token: func(string) (string, error) { return "credential", nil }}
	m := pairedManifest()
	m.Authority = &solutionhost.Authority{Principal: m.Alias, Setup: []modulecontract.ResolvedSetup{{Binding: "schemas", Methods: []string{"/store.v1.Store/Define"}, Scopes: []string{"records.types:define"}}}, OwnerAwareResourceKinds: []string{"example.items"}}
	_, err := d.Deliver(t.Context(), m)
	require.NoError(t, err)
	requests := peer.requests()
	require.Len(t, requests, 1)
	request := requests[0]
	require.NotNil(t, request.Authority)
	manifest, err := solutionhost.ManifestFromVerified(request.Carrier.Document)
	require.NoError(t, err)
	var policy compositionv1.AuthorityManifestV1
	require.NoError(t, proto.Unmarshal(request.Authority.Document, &policy))
	require.Equal(t, m.Authority.OwnerAwareResourceKinds, policy.OwnerAwareResourceKinds)
	require.Len(t, policy.Setup, 1)
	require.Equal(t, "schemas", policy.Setup[0].Binding)
	require.Equal(t, m.Authority.Setup[0].Methods, policy.Setup[0].Methods)
	require.Equal(t, m.Authority.Setup[0].Scopes, policy.Setup[0].Scopes)
	doc := documentOf(t, request.Authority.Document, manifest)
	doc.CoreManifestDigest = policy.CoreManifestDigest
	doc.OwnerAwareResourceKinds = policy.OwnerAwareResourceKinds
	for _, entry := range policy.Setup {
		doc.Setup = append(doc.Setup, modulecontract.ResolvedSetup{Binding: entry.Binding, Methods: entry.Methods, Scopes: entry.Scopes})
	}
	require.NoError(t, delivered.Pair(doc, request.Carrier.Document, manifest))
	doc.Setup[0].Methods = []string{"/store.v1.Store/Other"}
	require.Error(t, delivered.Pair(doc, request.Carrier.Document, manifest), "equal permission cannot hide changed exact method")
}
