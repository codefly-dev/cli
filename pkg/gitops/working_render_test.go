package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/workingrender"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// The 2026-11-12 finding (obin-ai/platform-obin#719): every module render
// wrote deployments/modules/<module> whatever --env it was for, and in a
// workspace whose staging delivers from deployments/modules on main, each Blue
// render overwrote staging's delivered tree in the checkout. Publishing Blue
// then required that tree to hold Blue's render, so it sat committed on a
// delivery branch, one merge from handing staging a Blue tree.
//
// A render is now scoped to its environment's working render: staging and
// blue sit side by side, neither touches the other's path, and no render
// writes a delivered tree, whatever environment that tree belongs to.
func TestRendersOfTwoEnvironmentsSitSideBySideAndNeverTouchADeliveredTree(t *testing.T) {
	installFakeAgents(t)
	ctx := context.Background()
	root := t.TempDir()
	require.NoError(t, copyTree(filepath.Join("testdata", "container-ports"), root))
	config := filepath.Join(root, resources.WorkspaceConfigurationName)
	data, err := os.ReadFile(config)
	require.NoError(t, err)
	blue := "  - name: blue\n    namespace: acme\n    registry:\n      url: registry.example.com/acme\n    gitops:\n      path: deployments/environments/blue\n"
	require.NoError(t, os.WriteFile(config, append(data, []byte(blue)...), 0o644))

	// Staging delivers from deployments/modules: its committed tree.
	delivered := filepath.Join(root, "deployments", "modules", "shop")
	require.NoError(t, os.MkdirAll(filepath.Join(delivered, "services", "api"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(delivered, InventoryFilename), []byte(`{"environment":"staging"}`+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(delivered, "services", "api", "deployment.yaml"), []byte("kind: Deployment\n"), 0o644))
	committed := snapshotTree(t, filepath.Join(root, "deployments"))

	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	render := func(environment string) RenderResult {
		t.Helper()
		env, err := orchestration.SelectEnvironment(workspace, environment)
		require.NoError(t, err)
		result, err := RenderModule(ctx, workspace, module, env, "acme-"+environment, nil)
		require.NoError(t, err)
		require.Equal(t, workingrender.Module(root, environment, "shop"), result.Path)
		return result
	}

	render("staging")
	staging := snapshotTree(t, workingrender.Module(root, "staging", "shop"))
	render("blue")
	require.Equal(t, staging, snapshotTree(t, workingrender.Module(root, "staging", "shop")), "blue's render touched staging's")
	require.Equal(t, committed, snapshotTree(t, filepath.Join(root, "deployments")), "a render wrote a delivered tree")

	owned := map[string]string{"staging": "deployments/modules/shop", "blue": "deployments/environments/blue/modules/shop"}
	for environment, ownedPath := range owned {
		inventory, err := LoadInventory(workingrender.Module(root, environment, "shop"))
		require.NoError(t, err)
		require.Equal(t, environment, inventory.Environment)
		require.Equal(t, ownedPath, inventory.OwnedPath)

		// Publish reads the render of its own environment, by its path.
		read, err := loadPublicationInventory(ctx, workspace, &PublishRequest{Module: "shop", Environment: environment},
			moduleRenderDestination(workspace, environment, "shop"), filepath.Dir(filepath.Dir(ownedPath)))
		require.NoError(t, err)
		require.Equal(t, inventory.Digest, read.Digest)
	}

	ignore, err := os.ReadFile(filepath.Join(workingrender.Root(root), ".gitignore"))
	require.NoError(t, err)
	require.Equal(t, "*\n", string(ignore), "a working render can never be committed")
}

// Publish reads its own environment's working render and nothing else: with
// only another environment's render in the checkout it refuses with the render
// that lifts it, leaving that render as it was, and once its own exists it
// plans from exactly that one.
func TestPublishReadsOnlyItsOwnEnvironmentsRender(t *testing.T) {
	ctx, workspace, _ := developerPublishWorkspace(t)
	installDeveloperSigning(t, &developerSigning{signer: &fakeSigner{nonce: true}})
	production := workingrender.Module(workspace.Dir(), "production", "payments")
	other := workingrender.Module(workspace.Dir(), "staging", "payments")
	require.NoError(t, os.MkdirAll(filepath.Dir(other), 0o755))
	require.NoError(t, os.Rename(production, other))
	before := snapshotTree(t, other)

	request := PublishRequest{Module: "payments", Environment: "production", Local: true, PromotionBranch: "codefly/promote-payments-production"}
	_, err := PlanPublish(ctx, workspace, &request)
	require.ErrorContains(t, err, "module payments has never been rendered for environment production")
	require.ErrorContains(t, err, "run `codefly deploy gitops render payments --env production` first")
	require.Equal(t, before, snapshotTree(t, other), "a refused publish touched another environment's render")

	require.NoError(t, os.Rename(other, production))
	inventory, err := LoadInventory(production)
	require.NoError(t, err)
	plan, err := PlanPublish(ctx, workspace, &request)
	require.NoError(t, err)
	require.Equal(t, "production", plan.Environment)
	require.Equal(t, inventory.OwnedPath, plan.Path, "the plan delivers the tree production's render owns")
}
