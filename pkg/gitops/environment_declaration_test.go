package gitops

import (
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// declaredWorkspace is the cell fixture with service-config and
// service-identity rows for shop/api in staging, at the given mode and
// principal.
func declaredWorkspace(t *testing.T, mode, principal string) (*resources.Workspace, *environments.Environment) {
	t.Helper()
	rows := "    service-config:\n      services:\n        shop/api: {values: {MODE: " + mode + "}}\n" +
		"    service-identity:\n      services:\n        shop/api: {principal: " + principal + ", annotations: {iam.gke.io/gcp-service-account: " + principal + "}}\n"
	workspace := writeCellWorkspace(t, [3]string{resources.WorkspaceConfigurationName, "    host:\n", rows + "    host:\n"})
	return workspace, selectedEnvironment(t, workspace, "staging")
}

// The digest covers the environment's service-config and service-identity rows
// for the module's own services: it moves when either row of shop/api moves,
// in any order of declaration, and not for billing, whose rows did not.
func TestTheDeclarationDigestFollowsTheModulesOwnRows(t *testing.T) {
	_, before := declaredWorkspace(t, "a", "shop@example.test")
	_, config := declaredWorkspace(t, "b", "shop@example.test")
	_, identity := declaredWorkspace(t, "a", "other@example.test")
	_, same := declaredWorkspace(t, "a", "shop@example.test")

	shop := environmentDeclarationDigest(before, "shop", []string{"api"})
	require.Equal(t, shop, environmentDeclarationDigest(same, "shop", []string{"api"}))
	require.NotEqual(t, shop, environmentDeclarationDigest(config, "shop", []string{"api"}), "a service-config value moved")
	require.NotEqual(t, shop, environmentDeclarationDigest(identity, "shop", []string{"api"}), "the service-identity moved")
	billing := environmentDeclarationDigest(before, "billing", []string{"ledger"})
	require.Equal(t, billing, environmentDeclarationDigest(config, "billing", []string{"ledger"}), "another module's rows are not billing's")
}

// A render for a target environment stamps the digest into its inventory, and
// a publish holds the tree to it: the same rows at publish admit it, moved
// rows refuse it by module and environment, and a tree rendered by a CLI that
// recorded no stamp is held to nothing here.
func TestAPublishRefusesATreeRenderedBeforeTheDeclarationMoved(t *testing.T) {
	workspace, env := declaredWorkspace(t, "a", "shop@example.test")
	result := renderCellTreeWith(t, &RenderOptions{
		Destination: moduleRenderDestination(workspace, "staging", "shop"),
		Module:      "shop", Environment: "staging", Namespace: "acme-shop", AppProject: "acme-staging",
		Promotable: true, OwnedPath: "deployments/modules/shop",
		Units:  unitsWithPorts("shop", "api"),
		Target: env,
	}, "api", "staging")
	inventory, err := LoadInventory(result.Path)
	require.NoError(t, err)
	require.Equal(t, environmentDeclarationDigest(env, "shop", []string{"api"}), inventory.EnvironmentDeclarationDigest)
	require.NoError(t, refuseStaleDeclaration(env, &inventory))

	_, moved := declaredWorkspace(t, "a", "rotated@example.test")
	err = refuseStaleDeclaration(moved, &inventory)
	require.ErrorContains(t, err, "the rendered tree of shop was rendered before environment staging's declaration for its services changed")
	require.ErrorContains(t, err, "render shop again for staging before publishing it")

	unstamped := inventory
	unstamped.EnvironmentDeclarationDigest = ""
	require.NoError(t, refuseStaleDeclaration(moved, &unstamped))

	// A render with no target environment records no stamp.
	plain := renderCellTree(t, workspace, "billing", "ledger", "acme-billing", "staging")
	billing, err := LoadInventory(plain.Path)
	require.NoError(t, err)
	require.Empty(t, billing.EnvironmentDeclarationDigest)
}
