package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// A presentation edit must reach the snapshot used by the next Running-hook
// delivery even when the composition adds no root. No agent is started here.
func TestExtensionRefreshesGroupsWithoutAddingRoots(t *testing.T) {
	ctx := t.Context()
	workspace, env := writeHostedNativeWorkspace(t)
	path := filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName)
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	declaration := strings.Replace(string(body), "modules:\n  - name: model\n  - name: ledger", "modules:\n  - name: ledger\nsolutions:\n  - name: model\n    path: modules/model\nsolution-groups:\n  - id: operations\n    title: Operations\n    order: 1\n    solutions: [model]", 1)
	require.NoError(t, os.WriteFile(path, []byte(declaration), 0o600))
	workspace, err = resources.LoadWorkspaceFromDir(ctx, workspace.Dir())
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "model")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "model")
	require.NoError(t, err)
	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.RunMode)
	require.NoError(t, err)
	identities, err := gitops.ResolveNativeIdentities(ctx, workspace, env)
	require.NoError(t, err)
	cluster := &nativeCluster{workspace: workspace, env: env, identities: identities}
	nativeClusterMu.Lock()
	previous := nativeClusterActive
	nativeClusterActive = cluster
	nativeClusterMu.Unlock()
	t.Cleanup(func() {
		nativeClusterMu.Lock()
		nativeClusterActive = previous
		nativeClusterMu.Unlock()
	})
	t.Chdir(workspace.Dir())
	baseline := extensionBaseline(ctx, workspace)
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(declaration, "title: Operations", "title: Revised", 1)), 0o600))
	require.NoError(t, extendRunningWorkspace(ctx, flow, baseline))
	group, ok := cluster.composed().SolutionGroupOf("model")
	require.True(t, ok)
	require.Equal(t, "Revised", group.Title)
	require.Equal(t, []string{"model/model"}, flow.Roots())
	require.Nil(t, cluster.authority, "refreshing declarations starts no replacement authority")
}

// The roots a reload admits are the ones neither the run nor the composition
// it started from knew: a root composed after the start joins, in the
// composition's order; a root the composition already named when a service or
// solution run began stays outside that run's closure, as does one the run
// starts itself.
func TestFreshRootsAreTheOnesComposedAfterTheRunStarted(t *testing.T) {
	known := map[string]bool{"saas/host": true, "crm/api": true}
	require.Equal(t, []string{"wiki/backend", "docs/documents"}, freshRoots(known, []string{"saas/host", "wiki/backend", "crm/api", "docs/documents"}))
	require.Nil(t, freshRoots(known, []string{"crm/api", "saas/host"}), "nothing composed after the start admits nothing")
	require.Nil(t, freshRoots(known, nil))
}

// The roots a reload withdraws are the ones the run starts that the
// composition no longer names, the pinned ones excepted: a service or
// solution run's command-line roots are never withdrawn, a workspace run pins
// none.
func TestGoneRootsAreTheDroppedOnesTheRunDidNotPin(t *testing.T) {
	running := []string{"crm/api", "wiki/backend", "docs/documents"}
	require.Equal(t, []string{"wiki/backend", "docs/documents"}, goneRoots(map[string]bool{}, running, []string{"crm/api"}))
	require.Equal(t, []string{"wiki/backend"}, goneRoots(map[string]bool{"docs/documents": true}, running, []string{"crm/api"}), "a pinned root stays")
	require.Nil(t, goneRoots(map[string]bool{}, running, running), "nothing dropped withdraws nothing")
}
