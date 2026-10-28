package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
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

// TestRenderModuleIsNotRefusedOverAnotherEnvironmentsExclusion is the render
// caller's half of the Blue defect, and it is the one a direct test of
// ValidateWorkspace cannot give: it fails if RenderModule stops passing the
// environment it was given, however correct the validator underneath is.
//
// It covers that one entry. The other three callers that pass a selection —
// orchestrate.go's second call, dev.go and solution.go — are reviewed source,
// not covered here.
//
// `stale` excludes shop/store while shop/api still consumes it and withdraws
// nothing, so that environment genuinely does not hold up against the
// workspace graph. Rendering `staging`, whose own declaration is clean, must
// still succeed — the exclusion graph is built from the workspace's CURRENT
// composition, so an environment imported before a module overlay added a
// service carries no withdrawal for it, and an operator cannot repair an
// environment the product intent does not declare.
func TestRenderModuleIsNotRefusedOverAnotherEnvironmentsExclusion(t *testing.T) {
	installFakeAgents(t)
	root := t.TempDir()
	require.NoError(t, copyTree(filepath.Join("testdata", "container-ports"), root))
	path := filepath.Join(root, resources.WorkspaceConfigurationName)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data = append(data, []byte(`  - name: stale
    namespace: acme
    registry:
      url: registry.example.com/acme
    exclude-services:
      - service: shop/store
`)...)
	require.NoError(t, os.WriteFile(path, data, 0o644))
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)

	// The other environment really is inconsistent with the graph: selecting
	// it is refused. Without this the next assertion would pass vacuously.
	stale, err := orchestration.SelectEnvironment(workspace, "stale")
	require.NoError(t, err)
	_, err = RenderModule(ctx, workspace, module, stale, "acme-stale", nil)
	require.ErrorContains(t, err, "shop/store",
		"the inconsistent environment must still be refused when it is the selected one")

	// Selecting the clean environment must not inherit that refusal.
	staging, err := orchestration.SelectEnvironment(workspace, "staging")
	require.NoError(t, err)
	result, err := RenderModule(ctx, workspace, module, staging, "acme-staging", nil)
	require.NoError(t, err, "a render is held to its own environment, never refused over another's")
	require.DirExists(t, filepath.Join(result.Path, "services", "store"),
		"and the service the OTHER environment excludes is rendered here")
}

// TestRenderModuleIsNotRefusedOverAnotherEnvironmentsScopedName is the Blue
// m106 defect, reproduced on released source: `deploy gitops render --all
// --env blue` failed with
//
//	environment "staging" service-config references ambiguous service "object-storage"
//
// A bare service-scoped name is resolved against the WORKSPACE's service
// graph, so it becomes ambiguous the moment a second module composes a
// service of that name. The declaration that goes stale belongs to whichever
// environment wrote the bare key first — here `staging` — but every
// environment was held to it, so a correct render of `blue` was refused over
// a declaration that is not blue's and that blue cannot repair.
//
// Here `depot` composes a second service named `store`, which makes staging's
// bare `store` key ambiguous. Rendering `blue`, whose own declarations are
// clean, must succeed.
func TestRenderModuleIsNotRefusedOverAnotherEnvironmentsScopedName(t *testing.T) {
	installFakeAgents(t)
	root := t.TempDir()
	require.NoError(t, copyTree(filepath.Join("testdata", "container-ports"), root))

	// A second module composing a service named "store": this is what makes
	// the bare name ambiguous across the workspace.
	depot := filepath.Join(root, "modules", "depot")
	require.NoError(t, os.MkdirAll(filepath.Join(depot, "services", "store"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(depot, "module.codefly.yaml"),
		[]byte("kind: module\nname: depot\nservices:\n  - name: store\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(depot, "services", "store", "service.codefly.yaml"),
		[]byte("kind: service\nname: store\nversion: 0.0.1\n"+
			"agent:\n  kind: codefly:service\n  name: postgres\n  version: 0.0.1\n  publisher: codefly.dev\n"+
			"endpoints:\n  - name: tcp\n    api: tcp\n"), 0o644))

	path := filepath.Join(root, resources.WorkspaceConfigurationName)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data = []byte(strings.Replace(string(data), "modules:\n  - name: shop\n",
		"modules:\n  - name: shop\n  - name: depot\n", 1))
	// staging carries the stale bare key; blue declares nothing of the sort.
	data = append(data, []byte(`    service-config:
      services:
        store:
          values:
            DATABASE_HOST: store.database.example
  - name: blue
    namespace: acme
    registry:
      url: registry.example.com/acme
`)...)
	require.NoError(t, os.WriteFile(path, data, 0o644))

	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)

	// staging really is ambiguous against the graph: selecting it refuses.
	// Without this the next assertion could pass for the wrong reason.
	staging, err := orchestration.SelectEnvironment(workspace, "staging")
	require.NoError(t, err)
	_, err = RenderModule(ctx, workspace, module, staging, "acme-staging", nil)
	require.ErrorContains(t, err, `references ambiguous service "store"`,
		"the environment that owns the stale declaration must still be refused")

	// blue must not inherit it.
	blue, err := orchestration.SelectEnvironment(workspace, "blue")
	require.NoError(t, err)
	_, err = RenderModule(ctx, workspace, module, blue, "acme-blue", nil)
	require.NoError(t, err, "a render is held to its own environment's declarations, never another's")
}

// A module the environment withdraws whole renders nothing there: the render
// refuses it by name instead of producing an empty tree.
func TestRenderModuleRefusesAModuleTheEnvironmentWithdrawsWhole(t *testing.T) {
	installFakeAgents(t)
	root := t.TempDir()
	require.NoError(t, copyTree(filepath.Join("testdata", "container-ports"), root))
	path := filepath.Join(root, resources.WorkspaceConfigurationName)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data = append(data, []byte(`    exclude-services:
      - service: shop/api
      - service: shop/store
`)...)
	require.NoError(t, os.WriteFile(path, data, 0o644))
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	env, err := orchestration.SelectEnvironment(workspace, "staging")
	require.NoError(t, err)
	require.True(t, env.WithdrawsModule(module))

	_, err = RenderModule(ctx, workspace, module, env, "acme-staging", nil)
	require.ErrorContains(t, err, "environment staging withdraws every service of module shop (exclude-services)")
}

// withdrawnCellWorkspace is the cell fixture with staging excluding shop/api
// and withdrawing billing/ledger's edge to it — and, as the exclusion
// requires, with no ingress, egress or cell declaration naming shop/api left.
func withdrawnCellWorkspace(t *testing.T) *resources.Workspace {
	t.Helper()
	workspace := writeCellWorkspace(t,
		[3]string{resources.WorkspaceConfigurationName, "    ingress:\n      - name: shop-http\n        service: shop/api\n        endpoint: http\n        hosts: [shop.example.test]\n", ""},
		[3]string{resources.WorkspaceConfigurationName, "    egress:\n      shop/api: {hosts: [identity.example.test, {name: api.github.com, port: 8443}]}\n", ""},
		[3]string{resources.WorkspaceConfigurationName, "    cell:\n      shop/api: {bindings: [vault, identity], cloud-identity: true, presenter: [http]}\n", `    exclude-services:
      - service: shop/api
        withdrawn-from: [billing/ledger]
`})
	require.NoError(t, environments.ValidateWorkspace(context.Background(), workspace), "the fixture is a state the exclusion admits")
	return workspace
}

// A withdrawn edge is no consumer in the cell file: the cell admits nothing to
// a workload that is not there.
func TestAWithdrawnEdgeIsNoCellConsumer(t *testing.T) {
	workspace := withdrawnCellWorkspace(t)
	consumers, _, err := endpointConsumers(context.Background(), workspace, selectedEnvironment(t, workspace, "staging"))
	require.NoError(t, err)
	require.Empty(t, consumers["shop/api/grpc"])
}

// A module the environment withdraws whole owes the cell file no render: the
// file is derived from the other modules' trees and describes nothing of it.
func TestCellFileLeavesOutAModuleTheEnvironmentWithdrawsWhole(t *testing.T) {
	workspace := withdrawnCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	shop, err := workspace.LoadModuleFromName(context.Background(), "shop")
	require.NoError(t, err)
	require.True(t, env.WithdrawsModule(shop), "the fixture withdraws every service of shop")
	renderCellTree(t, workspace, "billing", "ledger", "acme-billing", "staging")

	result, err := RenderCell(context.Background(), workspace, env)
	require.NoError(t, err)
	require.Equal(t, []string{"billing"}, result.Modules)
	file, err := os.ReadFile(result.Path)
	require.NoError(t, err)
	require.NotContains(t, string(file), "acme-shop")
}

// A cell file is never derived from a tree that still delivers an excluded
// service: the render of that module is owed first.
func TestCellFileRefusesATreeThatStillDeliversAnExcludedService(t *testing.T) {
	workspace := withdrawnCellWorkspace(t)
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
	require.ErrorContains(t, err, "from this environment (exclude-services")
	require.ErrorContains(t, err, "shop/api")

}
