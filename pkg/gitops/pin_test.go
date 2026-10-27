package gitops

import (
	"encoding/json"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// The render records how the composition pinned the module it rendered — the
// fact a cell's loader refuses a non-validation delivery on — and judges
// nothing itself.
func TestCompositionPinIsRecordedInTheInventory(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	workspace := &resources.Workspace{Modules: []*resources.ModuleReference{
		{Name: "saas", Source: "codefly-dev/module-saas-starter", Version: sha},
		{Name: "docs", Source: "obin-ai/module-documents", Version: "v0.3.1"},
		{Name: "search", Source: "obin-ai/module-search"},
		{Name: "wiki"},
	}}

	require.Equal(t, &InventoryPin{Source: "codefly-dev/module-saas-starter", Version: sha, Mode: PinModeCommit, Commit: sha}, compositionPin(workspace, "saas"))
	require.Equal(t, &InventoryPin{Source: "obin-ai/module-documents", Version: "v0.3.1", Mode: PinModeTag}, compositionPin(workspace, "docs"))
	require.Equal(t, &InventoryPin{Source: "obin-ai/module-search", Version: "latest", Mode: PinModeTag}, compositionPin(workspace, "search"))
	require.Nil(t, compositionPin(workspace, "wiki"), "a module composed from the workspace's own tree pins nothing")
	require.Nil(t, compositionPin(workspace, "absent"))
	require.Nil(t, compositionPin(nil, "saas"))

	inventory := inventoryHead(&RenderOptions{Module: "saas", Environment: "validation", Pin: compositionPin(workspace, "saas")})
	data, err := json.Marshal(inventory)
	require.NoError(t, err)
	require.Contains(t, string(data), `"pin":{"source":"codefly-dev/module-saas-starter","version":"`+sha+`","mode":"commit","commit":"`+sha+`"}`)

	var decoded Inventory
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, inventory.Pin, decoded.Pin)
	untouched := inventoryHead(&RenderOptions{Module: "wiki"})
	data, err = json.Marshal(untouched)
	require.NoError(t, err)
	require.NotContains(t, string(data), `"pin"`, "an unpinned module records no pin")
}
