package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// excludeStoreFixture is the container-ports fixture (api depends on store)
// with staging excluding store and withdrawing api's edge to it.
func excludeStoreFixture(t *testing.T) (*resources.Workspace, *resources.Module) {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, copyTree(filepath.Join("testdata", "container-ports"), root))
	path := filepath.Join(root, resources.WorkspaceConfigurationName)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data = append(data, []byte(`    exclude-services:
      - service: shop/store
        withdrawn-from: [shop/api]
`)...)
	require.NoError(t, os.WriteFile(path, data, 0o644))
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	return workspace, module
}

// An excluded service has no unit in the inventory and no tree in the render,
// and the module's other services render as before.
func TestRenderModuleLeavesAnExcludedServiceOutOfTheTreeAndTheInventory(t *testing.T) {
	installFakeAgents(t)
	workspace, module := excludeStoreFixture(t)
	env, err := orchestration.SelectEnvironment(workspace, "staging")
	require.NoError(t, err)

	result, err := RenderModule(context.Background(), workspace, module, env, "acme-staging", nil)
	require.NoError(t, err)

	var units []string
	for _, unit := range result.Inventory.Units {
		units = append(units, unit.Name)
	}
	require.Equal(t, []string{"api"}, units)
	require.DirExists(t, filepath.Join(result.Path, "services", "api"))
	require.NoDirExists(t, filepath.Join(result.Path, "services", "store"))

	// Publish admits the tree: the excluded service is owed no unit.
	require.NoError(t, validateModuleUnits(context.Background(), workspace, "shop", "staging", result.Inventory.Units))
	// And a tree that still carries it — rendered before the exclusion — is
	// refused by name rather than delivered.
	stale := append(result.Inventory.Units, InventoryUnit{Kind: UnitKindService, Module: "shop", Name: "store"})
	require.ErrorContains(t, validateModuleUnits(context.Background(), workspace, "shop", "staging", stale),
		`rendered unit "store" is a service environment "staging" excludes`)
}

// A withdrawn edge is no consumer in the cell file: the cell admits nothing to
// a workload that is not there.
func TestAWithdrawnEdgeIsNoCellConsumer(t *testing.T) {
	workspace := writeCellWorkspace(t, [3]string{resources.WorkspaceConfigurationName, "    host:\n", `    exclude-services:
      - service: shop/api
        withdrawn-from: [billing/ledger]
    host:
`})
	consumers, _, err := endpointConsumers(context.Background(), workspace, selectedEnvironment(t, workspace, "staging"))
	require.NoError(t, err)
	require.Empty(t, consumers["shop/api/grpc"])
}

// A cell file is never derived from a tree that still delivers an excluded
// service: the render of that module is owed first.
func TestCellFileRefusesATreeThatStillDeliversAnExcludedService(t *testing.T) {
	workspace := writeCellWorkspace(t, [3]string{resources.WorkspaceConfigurationName, "    host:\n", `    exclude-services:
      - service: shop/api
        withdrawn-from: [billing/ledger]
    host:
`})
	renderCellTree(t, workspace, "shop", "api", "acme-shop", "staging")
	_, err := RenderCell(context.Background(), workspace, selectedEnvironment(t, workspace, "staging"))
	require.ErrorContains(t, err, "the render of module shop still delivers shop/api, which environment staging excludes")
}

// A workspace configuration reference to an excluded service's endpoint would
// resolve to nothing, so the render's plan refuses it, naming the exclusion.
func TestPlanRefusesAConfigurationReferenceToAnExcludedService(t *testing.T) {
	configured := writeConfiguredWorkspace(t, "${endpoint:shop/api/http}")
	path := filepath.Join(configured.Dir(), resources.WorkspaceConfigurationName)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(data, []byte("    exclude-services:\n      - service: shop/api\n")...), 0o644))
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, configured.Dir())
	require.NoError(t, err)
	env, err := orchestration.SelectEnvironment(workspace, "staging")
	require.NoError(t, err)
	billing, err := workspace.LoadModuleFromName(ctx, "billing")
	require.NoError(t, err)
	ledger, err := billing.LoadServiceFromName(ctx, "ledger")
	require.NoError(t, err)

	err = orchestration.PlanConfigurationReferences(ctx, workspace, env, []*resources.Service{ledger}, false)
	require.ErrorContains(t, err, "the environment's exclude-services")
	require.ErrorContains(t, err, "shop/api")
}
