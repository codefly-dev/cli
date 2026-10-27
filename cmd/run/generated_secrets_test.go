package run

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/configurations"
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

// importingWorkspace is a workspace composing `base` by path, whose own
// configurations declare the hop group (MODULES, ENDPOINT) the way
// platform-core does; extension and authored add the importing workspace's own
// declarations.
func importingWorkspace(t *testing.T, extension, authored bool) *resources.Workspace {
	t.Helper()
	root := t.TempDir()
	declaration := "name: product\nlayout: modules\nworkspaces:\n    - name: base\n      path: ../base\nenvironments:\n    - name: local\n"
	if extension {
		declaration += "configuration-extensions:\n    hop: {}\n"
	}
	files := map[string]string{
		"base/workspace.codefly.yaml":       "name: base\nlayout: modules\nenvironments:\n    - name: local\n",
		"base/configurations/local/hop.env": "MODULES=annotations,robin\nENDPOINT=hop:1\n",
		"product/.gitignore":                "**/configurations/local/*.secret.env\n",
		"product/workspace.codefly.yaml":    declaration,
	}
	if authored {
		files["product/configurations/local/hop.env"] = "MODULES=robin\n"
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = filepath.Join(root, "product")
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git init: %s", out)
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), filepath.Join(root, "product"))
	require.NoError(t, err)
	return workspace
}

// A generated key for a group the workspace inherits from a composed
// workspace would make an own group that replaces the inherited one whole —
// the hop would lose MODULES and every route with it — so the run refuses,
// naming the declaration that makes the key extend the inherited group, and
// writes nothing. Declared, the generated key joins the inherited group. A
// group the workspace authors itself is its own to replace.
func TestAGeneratedSecretNeverReplacesAnInheritedGroup(t *testing.T) {
	ctx := context.Background()
	env := &environments.Environment{Name: "local", ServiceSecrets: &environments.EnvironmentServiceSecrets{
		Generate: []environments.EnvironmentSecretGenerator{{Scope: environments.SecretGeneratorScopeWorkspace, Configuration: "hop", Keys: []string{"PROOF"}}},
	}}
	read := func(t *testing.T, workspace *resources.Workspace) map[string]bool {
		t.Helper()
		provided, err := configurations.ReadWorkspaceConfigurations(ctx, workspace, &resources.Environment{Name: "local"})
		require.NoError(t, err)
		keys := map[string]bool{}
		for _, info := range provided.Infos {
			if info.GetName() == "hop" {
				for _, value := range info.GetConfigurationValues() {
					keys[value.GetKey()] = true
				}
			}
		}
		return keys
	}

	refused := importingWorkspace(t, false, false)
	_, err := provisionGeneratedSecrets(ctx, refused, env)
	require.ErrorContains(t, err, `workspace product inherits configuration "hop" from workspace base and generates PROOF into it`)
	require.ErrorContains(t, err, "configuration-extensions: {hop: {}}")
	require.NoFileExists(t, filepath.Join(refused.Dir(), "configurations", "local", "hop.secret.env"))
	require.Equal(t, map[string]bool{"MODULES": true, "ENDPOINT": true}, read(t, refused), "the inherited group is intact")

	extended := importingWorkspace(t, true, false)
	written, err := provisionGeneratedSecrets(ctx, extended, env)
	require.NoError(t, err)
	require.Equal(t, []string{"hop/PROOF"}, written)
	require.Equal(t, map[string]bool{"MODULES": true, "ENDPOINT": true, "PROOF": true}, read(t, extended), "the generated key extends the inherited group")

	authored := importingWorkspace(t, false, true)
	written, err = provisionGeneratedSecrets(ctx, authored, env)
	require.NoError(t, err)
	require.Equal(t, []string{"hop/PROOF"}, written)
}
