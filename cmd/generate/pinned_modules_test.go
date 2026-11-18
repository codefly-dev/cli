package generate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codefly-dev/core/resources"
)

// pinnedWorkspace is a workspace no command has materialized yet: its one
// module is a pin (a source and a version, here a tagged local repository) and
// neither the overlay nor the receipts exist, as on a fresh checkout or in a
// CI job. Core refuses to load that module until the CLI has pulled it.
func pinnedWorkspace(t *testing.T) string {
	t.Helper()
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())

	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	git("init", "--quiet")
	git("config", "user.email", "pinned@example.invalid")
	git("config", "user.name", "Pinned Test")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "modules", "documents"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "modules", "documents", resources.ModuleConfigurationName), []byte("name: documents\n"), 0o644))
	git("add", ".")
	git("-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "module")
	git("-c", "tag.gpgSign=false", "tag", "v0.0.1")

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(`name: pinned
layout: modules
module-resolution:
    documents: git
modules:
    - name: documents
      source: file://`+repo+`
      module: modules/documents
      version: v0.0.1
`), 0o644))

	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	require.NoError(t, err)
	_, err = workspace.LoadModuleFromName(context.Background(), "documents")
	require.ErrorContains(t, err, "resolves to a pinned artifact", "the fixture is a workspace whose module is still a pin")
	return root
}

// TestGenerateRunnablesPullsThePinnedModules: `generate runnables` loads
// composed modules (the one it is asked for, and every other one when a service
// declares an interface core binds across the composition), so it materializes
// the workspace's pins first, as `generate runnable-bindings` and `generate
// fixture-composition` do. It used the plain loader and refused, on a workspace
// no run had materialized, with core's "resolves to a pinned artifact".
func TestGenerateRunnablesPullsThePinnedModules(t *testing.T) {
	root := pinnedWorkspace(t)
	t.Chdir(root)
	resetRunnablesFlags(t)
	runnablesOutput = t.TempDir()

	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	require.FileExists(t, filepath.Join(runnablesOutput, "index.json"))
}

// TestGenerateContractsPullsThePinnedModules is the same for `generate
// contracts`, the verb a solution's --check gate runs first on a fresh
// checkout. The module exports nothing, so past the load the command refuses
// for that reason: what is asserted is that the pin is no longer what stops it.
func TestGenerateContractsPullsThePinnedModules(t *testing.T) {
	root := pinnedWorkspace(t)
	t.Chdir(root)
	resetContractsFlags(t)
	contractsOutput = t.TempDir()

	err := ContractsCmd.RunE(ContractsCmd, []string{"documents"})
	if err != nil {
		require.NotContains(t, err.Error(), "resolves to a pinned artifact")
		require.ErrorContains(t, err, "interface")
	}
	workspace, loadErr := resources.LoadWorkspaceFromDir(context.Background(), root)
	require.NoError(t, loadErr)
	module, loadErr := workspace.LoadModuleFromName(context.Background(), "documents")
	require.NoError(t, loadErr, "the command left the pinned module materialized")
	require.Equal(t, "documents", module.Name)
}
