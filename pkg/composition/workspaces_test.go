package composition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func workspaceFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func TestVersionedWorkspaceMaterializesOwnerModulesAndProductSolutions(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	coreRepo := t.TempDir()
	moduleURL := initModuleRepo(t, "module", "v1.0.0")
	workspaceFile(t, coreRepo, resources.WorkspaceConfigurationName, `name: platform-core
layout: modules
modules:
  - name: saas
    source: `+moduleURL+`
    module: module
    version: 1.0.0
module-resolution:
  saas: git
`)
	runGit(t, coreRepo, "init", "--quiet")
	runGit(t, coreRepo, "config", "user.email", "workspace@example.invalid")
	runGit(t, coreRepo, "config", "user.name", "Workspace Test")
	runGit(t, coreRepo, "add", ".")
	runGit(t, coreRepo, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "core release")
	runGit(t, coreRepo, "-c", "tag.gpgSign=false", "tag", "v1.0.0")
	// Real Git transport, redirected to the local release repository; no network
	// or replacement resolver hides the acquisition path under test.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url.file://"+coreRepo+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://github.com/team/platform-core.git")
	product := t.TempDir()
	declaration := `name: platform-obin
layout: modules
workspaces:
  - name: platform-core
    source: team/platform-core
    version: 1.0.0
solutions:
  - name: wiki
    source: ` + moduleURL + `
    module: module
    version: 1.0.0
  - name: lastlogin-go
    source: ` + moduleURL + `
    module: module
    version: 1.0.0
module-resolution:
  wiki: git
  lastlogin-go: git
`
	workspaceFile(t, product, resources.WorkspaceConfigurationName, declaration)
	readCtx := WithWorkspaceResolution(context.Background(), false)
	_, err := resources.LoadWorkspaceFromDir(readCtx, product)
	require.ErrorContains(t, err, "not materialized")
	ctx := WithWorkspaceResolution(context.Background(), true)
	ws, err := resources.LoadWorkspaceFromDir(ctx, product)
	require.NoError(t, err)
	require.Equal(t, []string{"saas", "wiki", "lastlogin-go"}, ws.ModulesNames())
	policy, err := EffectiveModuleResolutions(ws)
	require.NoError(t, err)
	for _, name := range ws.ModulesNames() {
		require.Equal(t, WorkspaceResolutionGit, policy[name])
	}
	require.NotEqual(t, product, ws.ModuleDeclarationDir("saas"))
	require.Equal(t, product, ws.ModuleDeclarationDir("wiki"))
	require.NoError(t, EnsurePinnedModules(ctx, ws))
	ws, err = resources.LoadWorkspaceFromDir(readCtx, product)
	require.NoError(t, err)
	for _, name := range ws.ModulesNames() {
		mod, err := ws.LoadModuleFromName(readCtx, name)
		require.NoError(t, err)
		require.Equal(t, name, mod.Name)
	}
	got, err := os.ReadFile(filepath.Join(product, resources.WorkspaceConfigurationName))
	require.NoError(t, err)
	require.Equal(t, declaration, string(got), "acquisition must not rewrite the declaration")
	// A missing upgrade fails closed, never selects the warmed old release.
	workspaceFile(t, product, resources.WorkspaceConfigurationName, strings.Replace(declaration, "version: 1.0.0", "version: 2.0.0", 1))
	_, err = resources.LoadWorkspaceFromDir(ctx, product)
	require.ErrorContains(t, err, "v2.0.0")
	_, err = resources.LoadWorkspaceFromDir(readCtx, product)
	require.ErrorContains(t, err, "not materialized")
}

func TestWorkspaceResolverRequiresAnExactRelease(t *testing.T) {
	for _, version := range []string{"latest", "main", "../escape", ">=1.0.0", "1.2"} {
		t.Run(version, func(t *testing.T) {
			root := t.TempDir()
			workspaceFile(t, root, resources.WorkspaceConfigurationName, "name: product\nlayout: modules\nworkspaces:\n  - name: platform-core\n    source: team/core\n    version: '"+version+"'\n")
			_, err := resources.LoadWorkspaceFromDir(WithWorkspaceResolution(context.Background(), true), root)
			require.ErrorContains(t, err, "exact semantic version")
		})
	}
}
