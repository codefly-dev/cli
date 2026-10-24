package run

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"

	"github.com/codefly-dev/cli/pkg/environments"
)

func generatedSecretsWorkspace(t *testing.T) *resources.Workspace {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		".gitignore":             "**/configurations/local/*.secret.env\n",
		"workspace.codefly.yaml": "name: secrets-fixture\nlayout: modules\nenvironments:\n    - name: local\n",
	}
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git init: %s", out)
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), dir)
	require.NoError(t, err)
	return workspace
}

// A local run generates what its environment's service-secrets declare and the
// workspace's secret configuration does not hold yet, into the file
// `codefly config generate` writes, and never rotates a held value.
func TestALocalRunGeneratesTheSecretsItsEnvironmentDeclares(t *testing.T) {
	workspace := generatedSecretsWorkspace(t)
	env := &environments.Environment{Name: "local", ServiceSecrets: &environments.EnvironmentServiceSecrets{
		Generate: []environments.EnvironmentSecretGenerator{
			{Scope: environments.SecretGeneratorScopeWorkspace, Configuration: "hop", Keys: []string{"PROOF"}},
			{Scope: environments.SecretGeneratorScopeWorkspace, Configuration: "cursors", Keys: []string{"KEY"}, Format: "base64", Bytes: 16},
		},
	}}
	written, err := provisionGeneratedSecrets(context.Background(), workspace, env)
	require.NoError(t, err)
	require.Equal(t, []string{"hop/PROOF", "cursors/KEY"}, written)

	path := filepath.Join(workspace.Dir(), "configurations", "local", "hop.secret.env")
	first, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Regexp(t, `^PROOF=[0-9a-f]{64}\n?$`, string(first))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	written, err = provisionGeneratedSecrets(context.Background(), workspace, env)
	require.NoError(t, err)
	require.Empty(t, written, "a held value is kept")
	again, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(first), string(again))
}

// An environment whose service-secrets names a store keeps its secrets there:
// a run writes nothing for it, and a service-scope generator naming no service
// is refused rather than guessed.
func TestALocalRunWritesNothingForAStoreAndGuessesNoService(t *testing.T) {
	workspace := generatedSecretsWorkspace(t)
	stored := &environments.Environment{Name: "local", ServiceSecrets: &environments.EnvironmentServiceSecrets{
		SecretStore: environments.EnvironmentSecretStoreReference{Name: "cell", Kind: "ClusterSecretStore"},
		Generate:    []environments.EnvironmentSecretGenerator{{Scope: environments.SecretGeneratorScopeWorkspace, Configuration: "hop", Keys: []string{"PROOF"}}},
	}}
	written, err := provisionGeneratedSecrets(context.Background(), workspace, stored)
	require.NoError(t, err)
	require.Empty(t, written)
	_, err = os.Stat(filepath.Join(workspace.Dir(), "configurations"))
	require.True(t, os.IsNotExist(err))

	unnamed := &environments.Environment{Name: "local", ServiceSecrets: &environments.EnvironmentServiceSecrets{
		Generate: []environments.EnvironmentSecretGenerator{{Scope: environments.SecretGeneratorScopeService, Configuration: "signing", Keys: []string{"SEED"}}},
	}}
	_, err = provisionGeneratedSecrets(context.Background(), workspace, unnamed)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "only for the services the generator names"), err.Error())
}
