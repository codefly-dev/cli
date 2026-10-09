package show

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSnapshotSelectionUsesPinnedImportsAndNoLocalReceipt(t *testing.T) {
	root := t.TempDir()
	base, product := filepath.Join(root, "base"), filepath.Join(root, "product")
	run := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		require.NoError(t, err, string(out))
		return strings.TrimSpace(string(out))
	}
	create := func(dir, manifest string) string {
		t.Helper()
		require.NoError(t, os.MkdirAll(dir, 0700))
		run(dir, "init", "-q")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"), []byte(manifest), 0600))
		run(dir, "add", "workspace.codefly.yaml")
		run(dir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "fixture")
		return run(dir, "rev-parse", "HEAD")
	}
	baseManifest := "name: base\nlayout: modules\nmodules:\n  - name: shared\n    source: example/shared\n    module: modules/shared\n    version: '1.0'\n"
	baseCommit := create(base, baseManifest)
	productManifest := "name: product\nlayout: modules\nworkspaces:\n  - name: base\n    path: ../base\n"
	productCommit := create(product, productManifest)
	require.NoError(t, os.WriteFile(filepath.Join(base, "workspace.codefly.yaml"), []byte(strings.ReplaceAll(baseManifest, "1.0", "9.0")), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(product, "codefly.local.yaml"), []byte("invalid: ["), 0600))
	report, err := snapshotSelection(context.Background(), product, productCommit, []string{base + "=" + baseCommit})
	require.NoError(t, err)
	require.Len(t, report.Modules, 1)
	require.Equal(t, "1.0", report.Modules[0].Version)
	require.Equal(t, base, report.Modules[0].DeclarationDirectory)
	require.Equal(t, "unavailable", report.Modules[0].Resolution.State)
	require.Equal(t, baseCommit, report.Workspaces[1].Revision)
	require.Equal(t, "snapshot", report.Workspaces[1].SourceMode)
	require.Equal(t, product, report.Directory)
	_, err = snapshotSelection(context.Background(), product, productCommit, nil)
	require.ErrorContains(t, err, "explicit snapshot commit")
	_, err = snapshotSelection(context.Background(), product, "HEAD", nil)
	require.ErrorContains(t, err, "immutable")
	_, err = snapshotSelection(context.Background(), product, productCommit, []string{base + "=" + baseCommit, root + "/unused=" + baseCommit})
	require.ErrorContains(t, err, "unused")
	// Core, not the snapshot adapter, rejects conflicting module declarations.
	productCommit = create(product, productManifest+"modules:\n  - name: shared\n    source: example/shared\n    module: modules/shared\n    version: '2.0'\n")
	_, err = snapshotSelection(context.Background(), product, productCommit, []string{base + "=" + baseCommit})
	require.ErrorContains(t, err, "conflicts")
}
