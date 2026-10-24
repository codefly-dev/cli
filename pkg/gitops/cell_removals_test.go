package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/gitops/cellfile"
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

	removals, err := holdCellToBase(ctx, workspace, env, repo, "main", base, "", nil)
	require.NoError(t, err, "a full render removes nothing")
	require.Empty(t, removals)

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

	_, err = holdCellToBase(ctx, workspace, env, repo, "main", base, "", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "namespace acme-billing (module billing, 2 workload(s))")
	require.Contains(t, err.Error(), "consumer billing/ledger of acme-shop/api/grpc")
	require.Contains(t, err.Error(), "--allow-cell-removal (billing)")

	_, err = holdCellToBase(ctx, workspace, env, repo, "main", base, "", []string{"shop"})
	require.Error(t, err, "naming the endpoint's module covers its consumer edge, but not billing's namespace")
	require.NotContains(t, err.Error(), "consumer billing/ledger")
	require.Contains(t, err.Error(), "namespace acme-billing")
	removals, err = holdCellToBase(ctx, workspace, env, repo, "main", base, "", []string{"billing"})
	require.NoError(t, err)
	require.Equal(t, []string{"namespace acme-billing (module billing, 2 workload(s))", "consumer billing/ledger of acme-shop/api/grpc"}, removals)

	empty, _ := cellBase(t, nil)
	require.NoError(t, os.Remove(filepath.Join(empty, filepath.FromSlash(deliveryCellPath("", "staging")))))
	runGit(t, empty, "commit", "-am", "no cell file")
	emptyBase := runGit(t, empty, "rev-parse", "HEAD")
	removals, err = holdCellToBase(ctx, workspace, env, empty, "main", emptyBase, "", nil)
	require.NoError(t, err, "a base with no cell file has nothing to hold the workspace's to")
	require.Empty(t, removals)
}
