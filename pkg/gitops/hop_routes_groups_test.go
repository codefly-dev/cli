package gitops

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/workingrender"
	"github.com/codefly-dev/core/composition"
	"github.com/stretchr/testify/require"
)

// addPlatformRoute publishes one more route on platform's first listener, as a
// module release that adds a method does: its document gains the path, and
// the catalog and the package manifest name the document's new digest.
func addPlatformRoute(t *testing.T, workspaceDir, path string) {
	t.Helper()
	module := filepath.Join(workspaceDir, "modules", "platform")
	docPath := filepath.Join(module, "contracts", "api", "first.json")
	before, err := os.ReadFile(docPath)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(before, &doc))
	doc["paths"].(map[string]any)[path] = map[string]any{"get": map[string]any{"responses": map[string]any{"200": map[string]any{"description": "ok"}}}}
	after, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(docPath, after, 0o600))
	oldDigest, newDigest := composition.APIContractDigest(before), composition.APIContractDigest(after)
	for _, name := range []string{composition.APIContractCatalogFileName, "module.package.codefly.yaml"} {
		file := filepath.Join(module, name)
		raw, err := os.ReadFile(file)
		require.NoError(t, err)
		require.Contains(t, string(raw), oldDigest)
		raw = []byte(strings.ReplaceAll(string(raw), oldDigest, newDigest))
		if name == composition.APIContractCatalogFileName {
			var catalog composition.APIContractCatalog
			require.NoError(t, json.Unmarshal(raw, &catalog))
			for i := range catalog.Endpoints {
				if catalog.Endpoints[i].Service == "first" {
					catalog.Endpoints[i].Routes = append(catalog.Endpoints[i].Routes, composition.APIContractRoute{Method: "GET", Path: path})
				}
			}
			raw, err = json.Marshal(catalog)
			require.NoError(t, err)
		}
		require.NoError(t, os.WriteFile(file, raw, 0o600))
	}
}

// The hop bakes the route table generated from its modules' published
// catalogs into its tree, so the table is a group its render records: a
// module that publishes a new method makes the hop's render stale, and the
// hop's publish is refused until it renders the method in.
func TestTheHopRecordsTheRouteTableItBakesIn(t *testing.T) {
	ctx := context.Background()
	workspace := hopRouteWorkspace(t, "public")
	env := selectedEnvironment(t, workspace, "local")
	hop := loadServices(t, workspace, "payments", "worker")

	before, err := workspaceConfigurationDigests(ctx, workspace, env, hop, nil)
	require.NoError(t, err)
	require.Contains(t, before, hopRoutesGroup, "the hop's render records the route table it bakes in")

	addPlatformRoute(t, workspace.Dir(), "/first/readable")
	after, err := workspaceConfigurationDigests(ctx, workspace, env, hop, nil)
	require.NoError(t, err)
	require.NotEqual(t, before[hopRoutesGroup], after[hopRoutesGroup], "a new method is a new route table")
	err = refuseStaleRender("payments", "local", before, after)
	require.Error(t, err)
	require.Contains(t, err.Error(), hopRoutesGroup)
}

// A module behind the hop feeds the hop's route table without consuming it:
// publishing it past a hop delivered at another table would leave its new
// methods unrouted at the hop. The publish of a fronted module is held to the
// hop's render as a consumer's is to a group it reads; the hop's own module,
// and a module the hop does not front, feed nothing.
func TestAFrontedModuleHoldsTheHopToItsRouteTable(t *testing.T) {
	ctx := context.Background()
	workspace := hopRouteWorkspace(t, "public")
	env := selectedEnvironment(t, workspace, "local")
	hop := loadServices(t, workspace, "payments", "worker")

	fed, err := hopRouteFeed(ctx, workspace, env, "platform")
	require.NoError(t, err)
	require.Contains(t, fed, hopRoutesGroup, "platform is published through the hop")
	own, err := hopRouteFeed(ctx, workspace, env, "payments")
	require.NoError(t, err)
	require.Empty(t, own, "the hop's own module is reached directly")

	// The base branch delivers the hop rendered before platform's new method.
	delivered, err := workspaceConfigurationDigests(ctx, workspace, env, hop, nil)
	require.NoError(t, err)
	repo := t.TempDir()
	baseInventory := filepath.Join(repo, deliveryModulesPath(""), "payments", InventoryFilename)
	require.NoError(t, os.MkdirAll(filepath.Dir(baseInventory), 0o755))
	require.NoError(t, writeCanonicalInventory(baseInventory, &Inventory{SchemaVersion: SchemaVersion, Module: "payments", Environment: "local", WorkspaceConfigurationDigests: delivered}))
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "-A"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "base"},
		{"update-ref", "refs/remotes/origin/main", "HEAD"},
	} {
		out, gitErr := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		require.NoError(t, gitErr, string(out))
	}

	addPlatformRoute(t, workspace.Dir(), "/first/readable")
	request := &PublishRequest{Module: "platform", Environment: "local"}
	_, err = holdBaseConsumers(ctx, workspace, env, request, repo, "main", "", "", &Inventory{Module: "platform", Environment: "local"})
	require.Error(t, err, "the hop on the base branch does not route platform's new method")
	require.Contains(t, err.Error(), "payments (group hop-routes)")

	// The hop rendered at the new table here lifts the refusal; the plan
	// still names it as the next publish.
	current, err := workspaceConfigurationDigests(ctx, workspace, env, hop, nil)
	require.NoError(t, err)
	dir := workingrender.Module(workspace.Dir(), "local", "payments")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, writeCanonicalInventory(filepath.Join(dir, InventoryFilename), &Inventory{SchemaVersion: SchemaVersion, Module: "payments", Environment: "local", WorkspaceConfigurationDigests: current}))
	stale, err := holdBaseConsumers(ctx, workspace, env, request, repo, "main", "", "", &Inventory{Module: "platform", Environment: "local"})
	require.NoError(t, err)
	require.Equal(t, []string{"payments (group hop-routes)"}, stale)

	// A rollback re-delivers an earlier tree, which feeds the hop nothing the
	// composition's sources say now.
	stale, err = holdBaseConsumers(ctx, workspace, env, request, repo, "main", "", "0123abcd", &Inventory{Module: "platform", Environment: "local"})
	require.NoError(t, err)
	require.Empty(t, stale)
}
