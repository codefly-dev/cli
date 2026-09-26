package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// writeManagedUnitWorkspace lays down a modules-layout workspace where "sessions"
// and "catalog" each own a service named "redis", and an environment whose
// managed-services block is managedDeclaration.
func writeManagedUnitWorkspace(t *testing.T, managedDeclaration string) *resources.Workspace {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		resources.WorkspaceConfigurationName: `name: acme
layout: modules
modules:
  - name: sessions
  - name: catalog
environments:
  - name: staging
    namespace: platform
` + managedDeclaration,
	}
	for _, module := range []string{"sessions", "catalog"} {
		files[filepath.Join("modules", module, resources.ModuleConfigurationName)] = `kind: module
name: ` + module + `
services:
    - name: redis
`
		files[filepath.Join("modules", module, "services", "redis", resources.ServiceConfigurationName)] = `kind: service
name: redis
version: 0.0.0
agent:
  kind: runtime::service
  name: go-grpc
  version: 0.0.1
  publisher: codefly.ai
`
	}
	for rel, content := range files {
		full := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	require.NoError(t, err)
	return workspace
}

// Publish admission compares each rendered unit's managed state against the
// environment, by the identity of the service the unit belongs to: an entry
// qualifying one module admits a bootstrap-only unit there and a rendered
// workload for the same-named service of the other module.
func TestValidateModuleUnitsAdmitsManagedStatePerModule(t *testing.T) {
	ctx := context.Background()
	workspace := writeManagedUnitWorkspace(t, `    managed-services:
      sessions/redis:
        kind: redis
        external-name: cache.internal.example
        port: 6379
`)

	require.NoError(t, validateModuleUnits(ctx, workspace, "sessions", "staging",
		[]InventoryUnit{{Kind: UnitKindService, Module: "sessions", Name: "redis", Managed: true}}))
	require.NoError(t, validateModuleUnits(ctx, workspace, "catalog", "staging",
		[]InventoryUnit{{Kind: UnitKindService, Module: "catalog", Name: "redis", Managed: false}}))

	// The states swapped: catalog/redis is not managed and sessions/redis is.
	require.Error(t, validateModuleUnits(ctx, workspace, "catalog", "staging",
		[]InventoryUnit{{Kind: UnitKindService, Module: "catalog", Name: "redis", Managed: true}}))
	require.Error(t, validateModuleUnits(ctx, workspace, "sessions", "staging",
		[]InventoryUnit{{Kind: UnitKindService, Module: "sessions", Name: "redis", Managed: false}}))
}

// A bare entry is workspace-wide, so both modules' units are managed — the
// behavior that was the only one available, and the reason an ambiguous bare key
// is refused at workspace load.
func TestValidateModuleUnitsAdmitsBareManagedEntryForEveryModule(t *testing.T) {
	ctx := context.Background()
	workspace := writeManagedUnitWorkspace(t, `    managed-services:
      redis:
        kind: redis
        external-name: cache.internal.example
        port: 6379
`)

	for _, module := range []string{"sessions", "catalog"} {
		require.NoError(t, validateModuleUnits(ctx, workspace, module, "staging",
			[]InventoryUnit{{Kind: UnitKindService, Module: module, Name: "redis", Managed: true}}))
	}
}
