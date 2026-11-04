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

// holdAgainst holds the publication's cell file to the base revision's, as a
// publication does: the base file read from the delivery repository, the
// claim given.
func holdAgainst(t *testing.T, ctx context.Context, cell *publicationCell, env *environments.Environment, repo, baseRevision string, allowed []string, claim *compositionClaim) (named, explained []string, err error) {
	t.Helper()
	base, err := baseCellFile(ctx, cell, repo, "main", baseRevision)
	require.NoError(t, err)
	return holdCellToBase(cell, env, "main", base, allowed, claim)
}

// advancedClaim is the claim of a checkout that has seen the base's
// composition: every module it does not declare was removed.
func advancedClaim(workspace *resources.Workspace) *compositionClaim {
	return &compositionClaim{declared: composedModules(workspace), advanced: true, revision: strings.Repeat("b", 40)}
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

	claim := advancedClaim(workspace)
	removals, explained, err := holdAgainst(t, ctx, publicationCellOf(t, workspace, env), env, repo, base, nil, claim)
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
	_, _, err = holdAgainst(t, ctx, dropping, env, repo, base, nil, claim)
	require.Error(t, err)
	require.Contains(t, err.Error(), "namespace acme-billing (module billing, 2 workload(s))")
	require.Contains(t, err.Error(), "consumer billing/ledger of acme-shop/api/grpc")
	require.Contains(t, err.Error(), "--allow-cell-removal (billing)")

	_, _, err = holdAgainst(t, ctx, dropping, env, repo, base, []string{"shop"}, claim)
	require.Error(t, err, "naming the endpoint's module covers its consumer edge, but not billing's namespace")
	require.NotContains(t, err.Error(), "consumer billing/ledger")
	require.Contains(t, err.Error(), "namespace acme-billing")
	removals, explained, err = holdAgainst(t, ctx, dropping, env, repo, base, []string{"billing"}, claim)
	require.NoError(t, err)
	require.Equal(t, []string{"namespace acme-billing (module billing, 2 workload(s))", "consumer billing/ledger of acme-shop/api/grpc"}, removals)
	require.Empty(t, explained, "billing is still composed: its removal is the flag's to confirm")

	empty, _ := cellBase(t, nil)
	require.NoError(t, os.Remove(filepath.Join(empty, filepath.FromSlash(deliveryCellPath("", "staging")))))
	runGit(t, empty, "commit", "-am", "no cell file")
	emptyBase := runGit(t, empty, "rev-parse", "HEAD")
	removals, _, err = holdAgainst(t, ctx, dropping, env, empty, emptyBase, nil, claim)
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

	named, explained, err := holdAgainst(t, ctx, publicationCellOf(t, workspace, env), env, repo, base, nil, advancedClaim(workspace))
	require.NoError(t, err, "a removal the composition explains needs no --allow-cell-removal")
	require.Empty(t, named)
	require.Equal(t, []string{
		"namespace acme-legacy (module legacy, 2 workload(s)): the composition no longer declares module legacy",
		"consumer legacy/worker of acme-shop/api/grpc: the composition no longer declares module legacy",
	}, explained)

	// The same cell file from a checkout that has not seen the base's
	// composition derives the same absence, and the composition explains
	// nothing: the removal is the flag's to confirm, and the refusal says why.
	stale := &compositionClaim{declared: composedModules(workspace), revision: strings.Repeat("c", 40)}
	_, _, err = holdAgainst(t, ctx, publicationCellOf(t, workspace, env), env, repo, base, nil, stale)
	require.Error(t, err)
	require.Contains(t, err.Error(), "--allow-cell-removal (legacy)")
	require.Contains(t, err.Error(), "does not contain "+strings.Repeat("c", 40))
	require.Contains(t, err.Error(), "may be a stale checkout's")
	_, _, err = holdAgainst(t, ctx, publicationCellOf(t, workspace, env), env, repo, base, nil, &compositionClaim{declared: composedModules(workspace)})
	require.Error(t, err)
	require.Contains(t, err.Error(), "names no revision it was rendered from")
	named, explained, err = holdAgainst(t, ctx, publicationCellOf(t, workspace, env), env, repo, base, []string{"legacy"}, stale)
	require.NoError(t, err, "naming the module confirms the removal as before")
	require.Len(t, named, 2)
	require.Empty(t, explained)
}

// The composition's claim is advanced only from a checkout that has seen the
// composition the base's cell file was rendered from. The workspace
// repository holding the delivery base commit is the exact test (the file's
// render was committed there); otherwise the revision the base file names is
// what the checkout must descend from; a base naming none advances nothing.
func TestTheCompositionClaimIsAdvancedOnlyFromACheckoutThatSawTheBase(t *testing.T) {
	ctx := context.Background()
	workspace := writeCellWorkspace(t)
	dir := workspace.Dir()
	runGit(t, dir, "init", "--initial-branch=main")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "the composition that named legacy")
	first := runGit(t, dir, "rev-parse", "HEAD")
	elsewhere := strings.Repeat("d", 40)
	// A delivery repository of its own, holding none of the workspace's history.
	separate := t.TempDir()
	runGit(t, separate, "init", "--initial-branch=main")
	runGit(t, separate, "commit", "-q", "--allow-empty", "-m", "delivery")

	base := &cellfile.File{Source: &cellfile.Source{Revision: first}}
	claim := newCompositionClaim(ctx, workspace, separate, "shop", elsewhere, base)
	require.True(t, claim.advanced, "a checkout at the revision the base was rendered from has seen its composition")
	require.Equal(t, first, claim.revision)
	require.True(t, claim.explains("legacy"))
	require.False(t, claim.explains("shop"), "a composed module is never explained away")
	require.False(t, claim.explains("billing"))
	require.False(t, newCompositionClaim(ctx, workspace, separate, "shop", elsewhere, nil).advanced, "a base with no cell file advances nothing")
	require.False(t, newCompositionClaim(ctx, workspace, separate, "shop", elsewhere, &cellfile.File{}).advanced, "a base naming no source advances nothing")

	// The workspace is the delivery repository: the base commit is the one
	// that committed the render, a child of the revision the file names. A
	// checkout at that revision has NOT seen it.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rendered.txt"), []byte("the render, and the modules: edit that rode with it\n"), 0o644))
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "render")
	second := runGit(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "checkout", "-q", "--detach", first)
	claim = newCompositionClaim(ctx, workspace, separate, "shop", second, base)
	require.False(t, claim.advanced, "the checkout does not contain the delivery base commit the workspace repository holds")
	require.Equal(t, second, claim.revision, "the base commit is judged, not the parent the file names")
	runGit(t, dir, "checkout", "-q", "main")
	require.True(t, newCompositionClaim(ctx, workspace, separate, "shop", second, base).advanced)

	// The same one repository, from a checkout that has not fetched the base
	// commit: the delivery clone holds the workspace revision the file names,
	// which is what tells one repository from two, so the base commit is still
	// the one to contain, and the checkout at the render's parent lacks it.
	clone := t.TempDir()
	runGit(t, "", "clone", "-q", dir, clone)
	runGit(t, dir, "checkout", "-q", "--detach", first)
	unfetched := strings.Repeat("f", 40)
	claim = newCompositionClaim(ctx, workspace, clone, "shop", unfetched, base)
	require.False(t, claim.advanced, "an unfetched checkout of the delivery repository is at the render's parent, not past it")
	require.Equal(t, unfetched, claim.revision)
	runGit(t, dir, "checkout", "-q", "main")

	// A checkout whose history does not contain the named revision at all.
	runGit(t, dir, "checkout", "-q", "--orphan", "stale")
	runGit(t, dir, "commit", "-q", "--allow-empty", "-m", "a branch that never saw it")
	require.False(t, newCompositionClaim(ctx, workspace, separate, "shop", elsewhere, base).advanced)
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
