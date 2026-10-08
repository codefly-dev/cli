package install

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/composition/pinnedfixture"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestInstallModulesRejectsIncompleteSignedPackageWithoutChangingSelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv(resources.CodeflyHomeEnv, home)
	pinnedfixture.AllowTempDirCleanup(t, home)
	fixture := pinnedfixture.New(t)
	fixture.AddRelease(t, "0.1.0")
	fixture.UseGitHub(t)
	dir := t.TempDir()
	pinnedfixture.AllowTempDirCleanup(t, dir)
	pinnedfixture.WriteWorkspace(t, dir, fixture)
	path := filepath.Join(dir, resources.WorkspaceConfigurationName)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	raw = append(raw, []byte("modules:\n  - name: saas\n    source: codefly-dev/module-saas-starter\n    version: 0.1.0\n")...)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	ws, err := resources.LoadWorkspaceFromDir(context.Background(), dir)
	require.NoError(t, err)
	require.ErrorContains(t, installWorkspaceModules(context.Background(), ws), "not loadable")
	requests := fixture.Requests()
	require.Positive(t, requests)
	require.ErrorContains(t, installWorkspaceModules(context.Background(), ws), "not loadable")
	require.Equal(t, requests, fixture.Requests(), "matching receipts must work without another fetch")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, raw, after, "materialization must preserve product selections")
	require.NoFileExists(t, filepath.Join(dir, "deployments"))
}

func TestInstallModulesFailsWhenSelectedPackageCannotBeMaterialized(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, resources.WorkspaceConfigurationName), []byte("name: test\nlayout: modules\nmodules:\n  - name: missing\n    source: example/missing\n    version: 1.0.0\n"), 0o600))
	ws, err := resources.LoadWorkspaceFromDir(context.Background(), dir)
	require.NoError(t, err)
	err = installWorkspaceModules(context.Background(), ws)
	require.ErrorContains(t, err, "missing")
	require.ErrorContains(t, err, "unmaterialized")
}

func TestInstallModulesPreservesLocalOverride(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "editable"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "editable", resources.ModuleConfigurationName), []byte("name: local\nversion: 1.0.0\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, resources.WorkspaceConfigurationName), []byte("name: test\nlayout: modules\nmodules:\n  - name: local\n    source: example/module\n    version: 1.0.0\n"), 0o600))
	overlay := []byte("resolve:\n  local:\n    path: ./editable\n")
	path := filepath.Join(dir, resources.LocalOverlayConfigurationName)
	require.NoError(t, os.WriteFile(path, overlay, 0o600))
	ws, err := resources.LoadWorkspaceFromDir(context.Background(), dir)
	require.NoError(t, err)
	require.NoError(t, installWorkspaceModules(context.Background(), ws))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, overlay, after)
	require.NoDirExists(t, filepath.Join(dir, "deployments"))
}
