package gitops

import (
	"context"
	"testing"

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
