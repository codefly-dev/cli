package run

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

const declaredLocalWorkspace = `name: declared-local
layout: flat
environments:
    - name: local
      naming-scope: from-yaml
      secrets:
          - kind: 1password
            account: acme-dev
`

func loadWorkspaceFixture(t *testing.T, content string) *resources.Workspace {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"), []byte(content), 0o600))
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), dir)
	require.NoError(t, err)
	return workspace
}

// Regression for #81: `codefly run service` synthesized a fresh local
// environment, so a workspace-declared local secret backend never reached
// the flow and op:// references failed to resolve.
func TestRunEnvironmentHonorsDeclaredLocal(t *testing.T) {
	workspace := loadWorkspaceFixture(t, declaredLocalWorkspace)

	env, err := runEnvironment(workspace)
	require.NoError(t, err)
	require.Equal(t, "from-yaml", env.NamingScope)
	require.Len(t, env.Secrets, 1)
	require.Equal(t, "1password", env.Secrets[0].Kind)
	require.Equal(t, "acme-dev", env.Secrets[0].Account)
}

func TestRunEnvironmentUndeclaredLocalKeepsLegacyDefault(t *testing.T) {
	workspace := loadWorkspaceFixture(t, "name: bare\nlayout: flat\n")

	env, err := runEnvironment(workspace)
	require.NoError(t, err)
	require.Equal(t, resources.LocalEnvironment(), env)
}

func TestRunEnvironmentNamingScopeOverrideStaysInvocationLocal(t *testing.T) {
	workspace := loadWorkspaceFixture(t, declaredLocalWorkspace)

	previous := namingScope
	t.Cleanup(func() { namingScope = previous })

	namingScope = "sdk-scope"
	env, err := runEnvironment(workspace)
	require.NoError(t, err)
	require.Equal(t, "sdk-scope", env.NamingScope)
	require.Len(t, env.Secrets, 1)

	require.Equal(t, "from-yaml", workspace.FindEnvironment("local").NamingScope)

	namingScope = ""
	fresh, err := runEnvironment(workspace)
	require.NoError(t, err)
	require.Equal(t, "from-yaml", fresh.NamingScope)
}
