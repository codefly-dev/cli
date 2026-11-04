package gitops

import (
	"testing"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	"github.com/codefly-dev/authority/libraries/authority-core/go/delivered"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// perClassDeclaration is ONE contract: robin required wherever shop is
// delivered, annotations optional on a development cell and required in
// production. It is written once and delivered twice below; nothing about it
// changes between the two environments.
const perClassDeclaration = `installation_role: installer
edge_dependencies:
  - module: robin
    presence: required
  - module: annotations
    presence_by_class: {development: optional, production: required}
`

// TestOneContractDeliversOptionalToDevelopmentAndRequiredToProduction is the
// proof the mechanism exists for: the same contract, rendered for an
// environment of class development and for one of class production, delivers
// `optional` to the first and `required` to the second.
//
// It reads BOTH signed carriers of the declaration, because the host compares
// them: the Core module manifest's authority block, which the render derives
// (moduleAuthority), and the authority document's AuthorityManifestV1, which
// the composition delivery sends (InstallationDeclaration). Resolving one and
// carrying the other verbatim would pass a one-carrier test and be refused at
// every install, so the pair is run through Authority's own agreement rule
// here too.
//
// And the delivered presence is a plain `required`/`optional` string: the
// host's vocabulary does not move, so accounts keeps deciding a dependency
// with `edge.Presence == "required"` and never learns what a class is.
func TestOneContractDeliversOptionalToDevelopmentAndRequiredToProduction(t *testing.T) {
	for class, want := range map[string]string{
		modulecontract.ClassDevelopment: modulecontract.PresenceOptional,
		modulecontract.ClassProduction:  modulecontract.PresenceRequired,
	} {
		t.Run(class, func(t *testing.T) {
			manifest, sidecar := producedCarriers(t, perClassDeclaration, class)

			// The Core manifest's half.
			require.Equal(t, []modulecontract.EdgeDependency{
				{Module: "robin", Presence: modulecontract.PresenceRequired},
				{Module: "annotations", Presence: want},
			}, manifest.Authority.EdgeDependencies)

			// The authority document's half, off the produced proto.
			var policy compositionv1.AuthorityManifestV1
			require.NoError(t, proto.Unmarshal(sidecar, &policy))
			require.Len(t, policy.GetEdgeDependencies(), 2)
			require.Equal(t, "annotations", policy.GetEdgeDependencies()[1].GetModule())
			require.Equal(t, want, policy.GetEdgeDependencies()[1].GetPresence())

			// And the two agree under Authority's real paired rule.
			doc := documentOf(t, sidecar, manifest)
			_, err := delivered.Module(manifest, &doc)
			require.NoError(t, err, "the two carriers disagree: one resolved the presence and the other did not")

			// A delivered manifest carries one presence and nothing of the
			// declaration it was resolved from; core's own manifest validation
			// refuses the other case, and here it holds on a real render.
			require.Nil(t, manifest.Authority.EdgeDependencies[1].PresenceByClass)
			require.NoError(t, manifest.Validate())
		})
	}
}

// TestAnEnvironmentWithNoClassIsRefusedByNameRatherThanDefaulted: a deliverer
// that has not said where it is delivering gets no default. The refusal names
// the module and the classes the contract does declare, on both carriers,
// because either one alone would let a render succeed and the install fail.
func TestAnEnvironmentWithNoClassIsRefusedByNameRatherThanDefaulted(t *testing.T) {
	workspace, module := writeAuthorityWorkspaceDeclaring(t, perClassDeclaration)
	env := selectedEnvironment(t, workspace, "staging")
	require.Empty(t, env.Class, "the test workspace's environment declares no class, which is the case under test")

	_, err := shopAuthority(t, workspace, module, loadServices(t, workspace, "shop", "api"), env)
	require.ErrorIs(t, err, modulecontract.ErrInvalid)
	require.Contains(t, err.Error(), "annotations")
	require.Contains(t, err.Error(), "declares no class")

	_, _, err = InstallationDeclaration(module.Dir(), env.Class)
	require.ErrorIs(t, err, modulecontract.ErrInvalid)
	require.Contains(t, err.Error(), "annotations")
	require.Contains(t, err.Error(), "development, production")
}

// TestAClassTheContractDoesNotNameIsRefused closes the other half: a staging
// cell delivered a contract that answers for development and production is
// refused naming staging, rather than taking whichever answer is nearest.
func TestAClassTheContractDoesNotNameIsRefused(t *testing.T) {
	workspace, module := writeAuthorityWorkspaceDeclaring(t, perClassDeclaration)
	env := selectedEnvironment(t, workspace, "staging")
	env.Class = modulecontract.ClassStaging

	_, err := shopAuthority(t, workspace, module, loadServices(t, workspace, "shop", "api"), env)
	require.ErrorIs(t, err, modulecontract.ErrInvalid)
	require.Contains(t, err.Error(), `"staging"`)
	require.Contains(t, err.Error(), "refused rather than resolved to a default")
}

// TestAPlainPresenceIsUnaffectedByTheEnvironmentClass is why no contract and no
// environment in the fleet has to change: every contract declares one presence
// for everywhere, and it reaches both carriers unchanged whatever the
// environment's class is, including none.
func TestAPlainPresenceIsUnaffectedByTheEnvironmentClass(t *testing.T) {
	plain := `installation_role: installer
edge_dependencies:
  - module: robin
    presence: required
  - module: annotations
    presence: optional
`
	for _, class := range append([]string{""}, modulecontract.EnvironmentClasses()...) {
		t.Run("class "+class, func(t *testing.T) {
			manifest, sidecar := producedCarriers(t, plain, class)
			require.Equal(t, []modulecontract.EdgeDependency{
				{Module: "robin", Presence: modulecontract.PresenceRequired},
				{Module: "annotations", Presence: modulecontract.PresenceOptional},
			}, manifest.Authority.EdgeDependencies)
			doc := documentOf(t, sidecar, manifest)
			_, err := delivered.Module(manifest, &doc)
			require.NoError(t, err)
		})
	}
}
