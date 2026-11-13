package gitops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/workingrender"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// The 2026-11-08 refusal: `deployments/modules/<module>` held another
// environment's render — a working-tree reset had put the committed one back,
// and the `render --all` that followed stopped at an earlier module — and
// publish reported what that render's unit graph disagreed about ("unexpected
// unit object-storage") instead of which render it was. The record is read
// first, so a stale tree names its environment and the render that lifts it.
func TestPublishNamesARenderForAnotherEnvironmentBeforeItsGraph(t *testing.T) {
	workspace := &resources.Workspace{Name: "platform"}
	workspace.WithDir(t.TempDir())
	rendered := workingrender.Module(workspace.Dir(), "blue", "saas")
	request := &PublishRequest{Module: "saas", Environment: "blue"}

	_, err := loadPublicationInventory(t.Context(), workspace, request, rendered, "")
	require.ErrorContains(t, err, "module saas has never been rendered for environment blue")
	require.ErrorContains(t, err, "run `codefly deploy gitops render saas --env blue` first")

	inventory := Inventory{SchemaVersion: SchemaVersion, Module: "saas", Environment: "staging", OwnedPath: "deployments/modules/saas",
		Units: []InventoryUnit{{Kind: UnitKindService, Module: "saas", Name: "accounts", Path: "services/accounts"}}}
	canonical, err := json.MarshalIndent(inventory, "", "  ")
	require.NoError(t, err)
	for _, unit := range []string{"accounts", "object-storage"} {
		require.NoError(t, os.MkdirAll(filepath.Join(rendered, "services", unit), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(rendered, InventoryFilename), append(canonical, '\n'), 0o644))

	_, err = loadPublicationInventory(t.Context(), workspace, request, rendered, "")
	require.ErrorContains(t, err, `holds the render of module "saas" for environment "staging"`)
	require.ErrorContains(t, err, "not module saas for environment blue")
	require.ErrorContains(t, err, "run `codefly deploy gitops render saas --env blue` first")
	require.NotContains(t, err.Error(), "unexpected unit")

	// Rendered for this environment by a render that succeeded, the tree is
	// still held to its graph.
	require.NoError(t, storeRenderRecord(workspace.Dir(), "blue", "saas", &renderRecord{Outcome: renderRecordRendered}))
	inventory.Environment = "blue"
	canonical, err = json.MarshalIndent(inventory, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(rendered, InventoryFilename), append(canonical, '\n'), 0o644))
	_, err = loadPublicationInventory(t.Context(), workspace, request, rendered, "")
	require.ErrorContains(t, err, "validate promotable render")
}
