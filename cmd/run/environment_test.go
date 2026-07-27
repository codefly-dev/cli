package run

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/orchestration"
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

func useRunEnvironment(t *testing.T, name string) {
	t.Helper()
	previous := environmentName
	t.Cleanup(func() { environmentName = previous })
	environmentName = name
}

// Regression for #81: `codefly run service` synthesized a fresh local
// environment, so a workspace-declared local secret backend never reached
// the flow and op:// references failed to resolve.
func TestRunEnvironmentHonorsDeclaredLocal(t *testing.T) {
	useRunEnvironment(t, orchestration.LocalEnvironmentName)
	workspace := loadWorkspaceFixture(t, declaredLocalWorkspace)

	env, err := runEnvironment(workspace)
	require.NoError(t, err)
	require.Equal(t, "from-yaml", env.NamingScope)
	require.Len(t, env.Secrets, 1)
	require.Equal(t, "1password", env.Secrets[0].Kind)
	require.Equal(t, "acme-dev", env.Secrets[0].Account)
}

func TestRunEnvironmentUndeclaredLocalKeepsLegacyDefault(t *testing.T) {
	useRunEnvironment(t, orchestration.LocalEnvironmentName)
	workspace := loadWorkspaceFixture(t, "name: bare\nlayout: flat\n")

	env, err := runEnvironment(workspace)
	require.NoError(t, err)
	require.Equal(t, resources.LocalEnvironment(), env)
}

func TestRunEnvironmentExplicitEmptyNamingScopeClearsDeclaredScope(t *testing.T) {
	useRunEnvironment(t, orchestration.LocalEnvironmentName)
	workspace := loadWorkspaceFixture(t, declaredLocalWorkspace)

	prevScope, prevExplicit := namingScope, namingScopeExplicit
	t.Cleanup(func() { namingScope, namingScopeExplicit = prevScope, prevExplicit })
	namingScope, namingScopeExplicit = "", true

	env, err := runEnvironment(workspace)
	require.NoError(t, err)
	require.Empty(t, env.NamingScope)
	require.Equal(t, "from-yaml", workspace.FindEnvironment("local").NamingScope)
}

// Pins the NewFlow call site, not just the selection helper: the flow built
// by `codefly run service` must carry the workspace-declared local
// environment all the way into orchestration.
func TestNewRunFlowCarriesDeclaredLocalEnvironment(t *testing.T) {
	useRunEnvironment(t, orchestration.LocalEnvironmentName)
	ctx := context.Background()
	dir := t.TempDir()
	files := map[string]string{
		"workspace.codefly.yaml": `name: run-env
layout: modules
modules:
    - name: web
environments:
    - name: local
      naming-scope: from-yaml
      secrets:
          - kind: 1password
            account: acme-dev
`,
		"modules/web/module.codefly.yaml": `kind: module
name: web
project: run-env
services:
    - name: gateway
`,
		"modules/web/services/gateway/service.codefly.yaml": `kind: service
name: gateway
version: 0.0.0
module: web
agent:
    kind: runtime::service
    name: krakend
    version: 0.0.6
    publisher: codefly.ai
endpoints:
    - name: rest
      visibility: public
      api: rest
`,
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	workspace, err := resources.LoadWorkspaceFromDir(ctx, dir)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "web")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "gateway")
	require.NoError(t, err)

	// An explicit runtime context skips the Docker probe so the flow builds
	// hermetically; no agent is spawned before InitManagers.
	prevContext, prevTemporaryPorts := runtimeContext, temporaryPorts
	t.Cleanup(func() {
		runtimeContext = prevContext
		temporaryPorts = prevTemporaryPorts
	})
	runtimeContext = resources.RuntimeContextNative
	temporaryPorts = true

	flow, err := newRunFlow(ctx, workspace, module, service)
	require.NoError(t, err)
	require.True(t, flow.TemporaryPortsEnabled())

	env := flow.Environment()
	require.NotNil(t, env)
	require.Equal(t, "local", env.Name)
	require.Equal(t, "from-yaml", env.NamingScope)
	require.Len(t, env.Secrets, 1)
	require.Equal(t, "1password", env.Secrets[0].Kind)
	require.Equal(t, "acme-dev", env.Secrets[0].Account)
}

func TestRunEnvironmentNamingScopeOverrideStaysInvocationLocal(t *testing.T) {
	useRunEnvironment(t, orchestration.LocalEnvironmentName)
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

func TestRunEnvironmentSelectsDeclaredProductionProfile(t *testing.T) {
	useRunEnvironment(t, "production")
	workspace := loadWorkspaceFixture(t, `name: production-run
layout: flat
environments:
    - name: local
      naming-scope: dev
    - name: production
      naming-scope: stable
      configuration-profile: local
`)

	env, err := runEnvironment(workspace)
	require.NoError(t, err)
	require.Equal(t, "production", env.Name)
	require.Equal(t, "stable", env.NamingScope)
	require.Equal(t, "local", env.ConfigurationProfile)
}

func TestRunEnvironmentRejectsUndeclaredNonLocalProfile(t *testing.T) {
	useRunEnvironment(t, "production")
	workspace := loadWorkspaceFixture(t, "name: local-only\nlayout: flat\n")

	env, err := runEnvironment(workspace)
	require.Nil(t, env)
	require.ErrorContains(t, err, `does not declare environment "production"`)
}
