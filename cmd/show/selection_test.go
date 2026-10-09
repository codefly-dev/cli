package show

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestSelectionReceiptRejectsStaleRequestsAndOverrides(t *testing.T) {
	ref := &resources.ModuleReference{Name: "shared", Source: "example/shared", Module: "module", Version: "1.2.3"}
	receipt := &composition.ResolutionReceipt{Source: ref.Source, Module: ref.Module, Requested: ref.Version, Version: ref.Version, Mode: composition.ResolutionModeVerified, Path: "/cache/materialized", Commit: "1234567890123456789012345678901234567890"}
	r := selectionReceipt(ref, nil, receipt, "")
	require.Equal(t, "matching-record", r.State)
	require.Equal(t, receipt.Commit, r.Commit)
	ref.Version = "1.2.4"
	r = selectionReceipt(ref, nil, receipt, "")
	require.Equal(t, "different-request", r.State)
	require.Equal(t, "1.2.3", r.Previous.Requested)
	require.Equal(t, "1.2.3", r.Previous.Version)
	require.Equal(t, "example/shared", r.Previous.Source)
	require.Empty(t, r.Version, "stale identity must not become the current resolution")
	encoded, err := json.Marshal(r)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "/cache/materialized")
	require.Empty(t, r.Commit)
	ref.Version = "1.2.3"
	r = selectionReceipt(ref, &resources.ModuleResolveDirective{Path: "/local/edit"}, receipt, "")
	require.Equal(t, "local-override", r.State)
	require.Empty(t, r.Commit)
	r = selectionReceipt(ref, &resources.ModuleResolveDirective{Path: receipt.Path}, receipt, "")
	require.Equal(t, "matching-record", r.State)
	r = selectionReceipt(ref, &resources.ModuleResolveDirective{Git: true}, receipt, "")
	require.Equal(t, "different-request", r.State)
	r = selectionReceipt(ref, nil, nil, "")
	require.Equal(t, "missing", r.State)
}

func TestSelectionAttachesReceiptThroughCanonicalLoader(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"), []byte("name: test\nlayout: modules\nmodules:\n  - name: shared\n    source: example/shared\n    module: module\n    version: '1.2.3'\n"), 0600))
	ctx := context.Background()
	ws, err := resources.LoadWorkspaceFromDir(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, composition.SaveResolutionReceipts(ctx, dir, map[string]*composition.ResolutionReceipt{"shared": {Source: "example/shared", Module: "module", Requested: "1.2.3", Version: "1.2.3", Mode: composition.ResolutionModeVerified, Path: "/private/cache"}}))
	report := selectionProjection(ws)
	attachSelectionReceipts(ctx, ws, &report)
	require.Equal(t, "matching-record", report.Modules[0].Resolution.State)
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "/private/cache")
}

func TestSelectionUsesCanonicalInheritanceAndProductEnvironments(t *testing.T) {
	root := t.TempDir()
	base, product := filepath.Join(root, "base"), filepath.Join(root, "product")
	require.NoError(t, os.MkdirAll(base, 0755))
	require.NoError(t, os.MkdirAll(product, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(base, "workspace.codefly.yaml"), []byte("name: base\nlayout: modules\nmodules:\n  - name: shared\n    source: example/shared\n    module: module\n    version: '1.2.3'\nenvironments:\n  - name: base-only\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(product, "workspace.codefly.yaml"), []byte("name: product\nlayout: modules\nworkspaces:\n  - name: base\n    path: ../base\nmodules:\n  - name: extra\n    source: example/extra\n    module: module\n    version: '4.5.6'\n"), 0600))
	ws, err := resources.LoadWorkspaceFromDir(context.Background(), product)
	require.NoError(t, err)
	report := selectionProjection(ws)
	require.Len(t, report.Modules, 2)
	require.Equal(t, product, report.Modules[0].DeclarationDirectory)
	require.Equal(t, base, report.Modules[1].DeclarationDirectory)
	require.Equal(t, "1.2.3", report.Modules[1].Version)
	require.Empty(t, report.Environments, "base environments must not become product environments")
	require.Equal(t, product, report.Workspaces[1].ParentDirectory)
	baseBytes, err := os.ReadFile(filepath.Join(base, "workspace.codefly.yaml"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(base, "workspace.codefly.yaml"), report.Workspaces[1].ManifestPath)
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(baseBytes)), report.Workspaces[1].ManifestSHA256)
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "configuration")
}
