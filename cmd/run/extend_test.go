package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"

	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
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
	cluster := &launcher{workspace: workspace, env: env, identities: identities}
	launcherMu.Lock()
	previous := launcherActive
	launcherActive = cluster
	launcherMu.Unlock()
	t.Cleanup(func() {
		launcherMu.Lock()
		launcherActive = previous
		launcherMu.Unlock()
	})
	t.Chdir(workspace.Dir())
	baseline := extensionBaseline(ctx, workspace)
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(declaration, "title: Operations", "title: Revised", 1)), 0o600))
	require.NoError(t, extendRunningWorkspace(ctx, flow, baseline))
	group, ok := cluster.composed().SolutionGroupOf("model")
	require.True(t, ok)
	require.Equal(t, "Revised", group.Title)
	require.Equal(t, []string{"model/model"}, flow.Roots())
	require.Nil(t, cluster.vouch, "refreshing declarations starts no replacement issuer")
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

// A stopped root has already left Flow.Roots when its module's withdrawal
// fails. The module stays pending, and a subsequent refresh attempts that
// withdrawal again, with no gone root to name it; the stopped workload itself
// is forgotten at once.
func TestWithdrawalRetriesAfterTheRootHasLeftTheFlow(t *testing.T) {
	ctx := t.Context()
	workspace, env := writeHostedNativeWorkspace(t)
	module, err := workspace.LoadModuleFromName(ctx, "model")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "model")
	require.NoError(t, err)
	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.RunMode)
	require.NoError(t, err)
	host := *env.Host
	host.Gateway = "model/model"
	hosted := *env
	hosted.Host = &host
	delivered := &solutionhost.ModuleManifest{
		Schema: solutionhost.SchemaModuleManifestV1, Binding: "acme.local.gone", Alias: "gone", Kind: solutionhost.KindModule, Generation: 1,
		Host: solutionhost.HostTarget{Coordinate: host.Coordinate, Domain: host.Domain}, Release: solutionhost.Release{Publisher: "acme", Name: "gone", Version: "1.0.0"},
		Workloads: []solutionhost.Workload{{Service: "backend", Container: "backend"}},
	}
	cluster := &launcher{
		stateDir: t.TempDir(), env: &hosted, flow: flow, workspace: workspace,
		identities:         map[string]gitops.NativeIdentity{"gone/backend": {Module: "gone", Service: "backend"}},
		running:            map[string]bool{"gone/backend": true},
		delivered:          map[string]*solutionhost.ModuleManifest{"gone": delivered},
		pendingWithdrawals: map[string]bool{},
	}
	launcherMu.Lock()
	previous := launcherActive
	launcherActive = cluster
	launcherMu.Unlock()
	t.Cleanup(func() { launcherMu.Lock(); launcherActive = previous; launcherMu.Unlock() })
	t.Chdir(workspace.Dir())

	withdrawn, err := cluster.withdraw(ctx, []string{"gone/backend"})
	require.Error(t, err, "the host's delivery endpoint is not resolved in this run")
	require.Empty(t, withdrawn)
	require.NotContains(t, cluster.identities, "gone/backend", "the stopped workload is forgotten")
	require.Empty(t, cluster.startedIdentities("gone"))
	require.True(t, cluster.pendingWithdrawals["gone"], "the module's withdrawal is retained for the next refresh")
	require.Same(t, delivered, cluster.delivered["gone"], "nothing was delivered in its place")

	require.NotContains(t, flow.Roots(), "gone/backend")
	require.Error(t, withdrawDroppedRoots(ctx, flow, pinnedRoots(flow)), "a refresh with no gone root retries the pending withdrawal")
	// A module this run never delivered has nothing to withdraw: the retry
	// succeeds and clears the pending withdrawal.
	delete(cluster.delivered, "gone")
	require.NoError(t, withdrawDroppedRoots(ctx, flow, pinnedRoots(flow)))
	require.Empty(t, cluster.pendingWithdrawals)
}

func TestRefreshPreflightKeepsBaselineRootsOutAndPrecedesAdmission(t *testing.T) {
	ctx := t.Context()
	workspace, env := writeHostedNativeWorkspace(t)
	module, err := workspace.LoadModuleFromName(ctx, "model")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "model")
	require.NoError(t, err)
	refusal := errors.New("invalid refreshed permissions")
	reject := false
	checks := 0
	check := func(_ context.Context, selected *resources.Workspace, _ *environments.Environment) error {
		checks++
		for _, ref := range selected.Modules {
			require.NotEqual(t, "ledger", ref.Name, "baseline-only roots must stay outside this run")
		}
		if reject {
			return refusal
		}
		return nil
	}
	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.RunMode, orchestration.WithRunModuleClosure("model"), orchestration.WithCompositionCheck(check))
	require.NoError(t, err)
	baseline := extensionBaseline(ctx, workspace)
	cluster := &launcher{workspace: workspace, env: env}
	launcherMu.Lock()
	previous := launcherActive
	launcherActive = cluster
	launcherMu.Unlock()
	t.Cleanup(func() { launcherMu.Lock(); launcherActive = previous; launcherMu.Unlock() })
	t.Chdir(workspace.Dir())
	reject = true
	require.ErrorIs(t, extendRunningWorkspace(ctx, flow, baseline), refusal, "even a no-new-root refresh is checked")
	require.Equal(t, 2, checks)
	require.Same(t, workspace, cluster.workspace, "admission did not change the launcher snapshot")
	composeModule(t, workspace, "newcomer")
	require.ErrorIs(t, extendRunningWorkspace(ctx, flow, baseline), refusal)
	require.Equal(t, 3, checks)
	require.Same(t, workspace, cluster.workspace)
	require.Equal(t, []string{"model/model"}, flow.Roots())
}
