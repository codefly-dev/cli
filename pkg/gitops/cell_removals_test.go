package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops/cellfile"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// cellBase is a delivery repository whose base revision carries the cell file
// given, at the path a publication reads it from.
func cellBase(t *testing.T, data []byte) (string, string) {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "--initial-branch=main")
	path := filepath.Join(repo, filepath.FromSlash(deliveryCellPath("", "staging")))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, data, 0o644))
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "base")
	return repo, runGit(t, repo, "rev-parse", "HEAD")
}

// publicationCellOf is the cell file a publication of this workspace carries,
// read as the publication reads it: the one the hold is made against and the
// one carried into the promotion are the same bytes.
func publicationCellOf(t *testing.T, workspace *resources.Workspace, env *environments.Environment) *publicationCell {
	t.Helper()
	cell, err := publicationCellFile(workspace, env, "")
	require.NoError(t, err)
	require.NotNil(t, cell, "the workspace renders a cell file for %s", env.Name)
	return cell
}

// A publication holds the workspace's cell file to the one its delivery base
// branch carries: a file that drops a module's namespace (and with it that
// module's consumer edges on another's endpoints) is refused, naming each
// removal and the module to name; naming it lets the removal through and the
// plan shows it; a file that removes nothing, or a base with no cell file,
// publishes as before.
func TestPublishRefusesACellFileThatRemovesWhatTheBaseAdmits(t *testing.T) {
	ctx := context.Background()
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	renderCellTree(t, workspace, "shop", "api", "acme-shop", "staging")
	renderCellTree(t, workspace, "billing", "ledger", "acme-billing", "staging")
	full, err := RenderCell(ctx, workspace, env)
	require.NoError(t, err)
	complete, err := os.ReadFile(full.Path)
	require.NoError(t, err)
	repo, base := cellBase(t, complete)

	declared := composedModules(workspace)
	removals, explained, err := holdCellToBase(ctx, publicationCellOf(t, workspace, env), env, repo, "main", base, nil, declared)
	require.NoError(t, err, "a full render removes nothing")
	require.Empty(t, removals)
	require.Empty(t, explained)

	// The cell file a render of shop alone wrote before it refused to: billing
	// is gone, and so is billing/ledger as a caller of shop's grpc endpoint.
	file, err := cellfile.Parse(complete)
	require.NoError(t, err)
	require.Equal(t, "acme-billing", file.Namespaces[0].Name)
	file.Namespaces = file.Namespaces[1:]
	shopAPI := &file.Namespaces[0].Workloads[0]
	require.Equal(t, []string{"billing/ledger", "shop/api"}, shopAPI.Endpoints[0].Consumers)
	shopAPI.Endpoints[0].Consumers = []string{"shop/api"}
	dropped, err := file.Encode()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(full.Path, dropped, 0o600))

	dropping := publicationCellOf(t, workspace, env)
	_, _, err = holdCellToBase(ctx, dropping, env, repo, "main", base, nil, declared)
	require.Error(t, err)
	require.Contains(t, err.Error(), "namespace acme-billing (module billing, 2 workload(s))")
	require.Contains(t, err.Error(), "consumer billing/ledger of acme-shop/api/grpc")
	require.Contains(t, err.Error(), "--allow-cell-removal (billing)")

	_, _, err = holdCellToBase(ctx, dropping, env, repo, "main", base, []string{"shop"}, declared)
	require.Error(t, err, "naming the endpoint's module covers its consumer edge, but not billing's namespace")
	require.NotContains(t, err.Error(), "consumer billing/ledger")
	require.Contains(t, err.Error(), "namespace acme-billing")
	removals, explained, err = holdCellToBase(ctx, dropping, env, repo, "main", base, []string{"billing"}, declared)
	require.NoError(t, err)
	require.Equal(t, []string{"namespace acme-billing (module billing, 2 workload(s))", "consumer billing/ledger of acme-shop/api/grpc"}, removals)
	require.Empty(t, explained, "billing is still composed: its removal is the flag's to confirm")

	empty, _ := cellBase(t, nil)
	require.NoError(t, os.Remove(filepath.Join(empty, filepath.FromSlash(deliveryCellPath("", "staging")))))
	runGit(t, empty, "commit", "-am", "no cell file")
	emptyBase := runGit(t, empty, "rev-parse", "HEAD")
	removals, _, err = holdCellToBase(ctx, dropping, env, empty, "main", emptyBase, nil, declared)
	require.NoError(t, err, "a base with no cell file has nothing to hold the workspace's to")
	require.Empty(t, removals)
}

// A removal the composition explains needs no naming. Once legacy leaves
// `modules:`, a render of the composition derives a cell file without it — and
// prunes its record — and a publication of that file passes the hold with no
// flag: the namespace and legacy's consumer edge on shop's endpoint are
// reported as removals the composition explains, for the reviewer, rather than
// refused until someone names a module the workspace no longer composes.
func TestARemovalTheCompositionExplainsNeedsNoNaming(t *testing.T) {
	ctx := context.Background()
	workspace := writeCellWorkspace(t)
	workspace = composeLegacyModule(t, workspace)
	env := selectedEnvironment(t, workspace, "staging")
	renderCellTree(t, workspace, "shop", "api", "acme-shop", "staging")
	renderCellTree(t, workspace, "billing", "ledger", "acme-billing", "staging")
	renderCellTree(t, workspace, "legacy", "worker", "acme-legacy", "staging")
	full, err := RenderCell(ctx, workspace, env)
	require.NoError(t, err)
	require.Equal(t, []string{"billing", "legacy", "shop"}, full.Modules)
	complete, err := os.ReadFile(full.Path)
	require.NoError(t, err)
	repo, base := cellBase(t, complete)

	// legacy leaves the composition; its module directory and its render
	// record are still in the checkout, as they are after a `modules:` edit.
	workspace = editWorkspaceConfiguration(t, workspace, "  - name: legacy\n", "")
	env = selectedEnvironment(t, workspace, "staging")

	rendered, err := RenderCell(ctx, workspace, env)
	require.NoError(t, err)
	require.Equal(t, []string{"billing", "shop"}, rendered.Modules)
	require.Equal(t, []string{"deployments/modules/legacy"}, rendered.Removed)
	require.NoDirExists(t, moduleRenderDestination(workspace, "legacy"))

	named, explained, err := holdCellToBase(ctx, publicationCellOf(t, workspace, env), env, repo, "main", base, nil, composedModules(workspace))
	require.NoError(t, err, "a removal the composition explains needs no --allow-cell-removal")
	require.Empty(t, named)
	require.Equal(t, []string{
		"namespace acme-legacy (module legacy, 2 workload(s)): the composition no longer declares module legacy",
		"consumer legacy/worker of acme-shop/api/grpc: the composition no longer declares module legacy",
	}, explained)
}

// composeLegacyModule adds a third module, legacy, to the cell fixture: one
// service, worker, calling shop's api, so dropping the module later removes a
// namespace and a consumer edge at once.
func composeLegacyModule(t *testing.T, workspace *resources.Workspace) *resources.Workspace {
	t.Helper()
	files := map[string]string{
		filepath.Join("modules", "legacy", resources.ModuleConfigurationName):                        "kind: module\nname: legacy\nservices:\n  - name: worker\n",
		filepath.Join("modules", "legacy", "services", "worker", resources.ServiceConfigurationName): cellServiceYAML("worker") + "service-dependencies:\n  - name: api\n    module: shop\n    endpoints:\n      - name: grpc\n",
	}
	for rel, content := range files {
		full := filepath.Join(workspace.Dir(), rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	return editWorkspaceConfiguration(t, workspace, "  - name: billing\n", "  - name: billing\n  - name: legacy\n")
}

// editWorkspaceConfiguration replaces one passage of the workspace's
// configuration and loads the workspace again.
func editWorkspaceConfiguration(t *testing.T, workspace *resources.Workspace, old, replacement string) *resources.Workspace {
	t.Helper()
	path := filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName)
	config, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(config), old)
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(config), old, replacement, 1)), 0o644))
	edited, err := resources.LoadWorkspaceFromDir(context.Background(), workspace.Dir())
	require.NoError(t, err)
	return edited
}
