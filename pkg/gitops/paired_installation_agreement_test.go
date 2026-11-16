package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	"github.com/codefly-dev/authority/libraries/solutionhost/go"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// agree holds the CLI's two carriers to declaring the same installation
// selector and the same edges, in order: what the host pairs them on
// (authority-core delivered.Module, proven there). An empty list and nil are
// the same zero edges.
func agree(t *testing.T, manifest *solutionhost.ModuleManifest, sidecar []byte) {
	t.Helper()
	var policy compositionv1.AuthorityManifestV1
	require.NoError(t, proto.Unmarshal(sidecar, &policy))
	require.Equal(t, manifest.Authority.InstallationRole, policy.GetInstallationRole(), "the two carriers declare different installation selectors")
	require.Len(t, policy.GetEdgeDependencies(), len(manifest.Authority.EdgeDependencies), "the two carriers declare different edges")
	for i, edge := range manifest.Authority.EdgeDependencies {
		require.Equal(t, edge.Module, policy.GetEdgeDependencies()[i].GetModule())
		require.Equal(t, edge.Presence, policy.GetEdgeDependencies()[i].GetPresence())
	}
}

// producedCarriers renders a module declaring `declaration` for an environment
// of class class (empty: an environment declaring none, which is every
// environment today) and returns the two signed carriers' declaration content:
// the Core manifest's Authority block and the AuthorityManifestV1 bytes, each
// from the real producer.
func producedCarriers(t *testing.T, declaration, class string) (*solutionhost.ModuleManifest, []byte) {
	t.Helper()
	workspace, module := writeAuthorityWorkspaceDeclaring(t, declaration)
	env := selectedEnvironment(t, workspace, "staging")
	env.Class = class
	_, manifest := renderShopManifest(t, workspace, module, env)

	// The same reader the delivery uses, over the same contract.
	role, edges, err := InstallationDeclaration(module.Dir(), uncomposed(class))
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
		"a named selector with ordered edges": {`installation_role: admin
edge_dependencies:
  - module: sidekick
    presence: required
  - module: annotations
    presence: optional
`, "admin", [][2]string{{"sidekick", "required"}, {"annotations", "optional"}}, true},
		"the none selector alone is reason enough for a sidecar": {
			"installation_role: none\n", "none", nil, true},
		"edges alone are reason enough for a sidecar": {`edge_dependencies:
  - module: sidekick
    presence: required
`, "", [][2]string{{"sidekick", "required"}}, true},
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
					return InstallationDeclaration(dir, uncomposed(""))
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
			return InstallationDeclaration(dir, uncomposed(""))
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
  - module: sidekick
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

// TestTheProducedCarriersAgree holds the CLI's OWN two carriers, as the
// producer emits them, to declaring the same installation selector and edges:
// what the host pairs them on. The pairing rule and its refusals are the
// authority's, proven in authority-core (delivered).
func TestTheProducedCarriersAgree(t *testing.T) {
	for name, declaration := range map[string]string{
		"a named selector with ordered edges": `installation_role: admin
edge_dependencies:
  - module: sidekick
    presence: required
  - module: annotations
    presence: optional
`,
		"the none selector": "installation_role: none\n",
		"no declaration":    "",
		"edges without a selector": `edge_dependencies:
  - module: sidekick
    presence: required
`,
	} {
		t.Run(name, func(t *testing.T) {
			manifest, sidecar := producedCarriers(t, declaration, "")
			agree(t, manifest, sidecar)
		})
	}
}

// nil and [] are equivalent zero edges on BOTH carriers: the producer delivers
// nil for an empty list precisely so the two spellings cannot disagree.
func TestNilAndEmptyEdgesAgree(t *testing.T) {
	manifest, sidecar := producedCarriers(t, "installation_role: none\n", "")
	require.Nil(t, manifest.Authority.EdgeDependencies, "the producer delivers nil for no edges")
	agree(t, manifest, sidecar)
}
