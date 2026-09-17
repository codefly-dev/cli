package run

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/runners/recoveryscope"
	"github.com/stretchr/testify/require"
)

const (
	recoveryWorkspaceYAML = `name: recovery
layout: modules
modules:
    - name: app
`
	recoveryModuleYAML = `kind: module
name: app
services:
    - name: api
`
	recoveryServiceYAML = `kind: service
name: api
version: 0.0.0
module: app
agent:
    kind: codefly:service
    name: absent
    version: 0.0.1
    publisher: codefly.dev
`
)

// loadRecoveryFixture lays down a workspace whose only service names an agent
// that cannot resolve, so nothing past the step under test can succeed. An
// empty preferences string writes no preferences file at all.
func loadRecoveryFixture(t *testing.T, preferences string) (*resources.Workspace, *resources.Module, *resources.Service) {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"workspace.codefly.yaml":                        recoveryWorkspaceYAML,
		"modules/app/module.codefly.yaml":               recoveryModuleYAML,
		"modules/app/services/api/service.codefly.yaml": recoveryServiceYAML,
	}
	if preferences != "" {
		files[resources.UserPreferencesFile] = preferences
	}
	for rel, content := range files {
		path := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "app")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "api")
	require.NoError(t, err)
	return workspace, module, service
}

// Ownership must reach an agent on every selection, not only the ones the CLI
// sweeps for. A native or Nix service still reaches Docker through a Core
// companion, and a container it creates without the recovery labels is
// invisible to every later sweep.
func TestRunProjectsContainerRecoveryForEverySelection(t *testing.T) {
	useRunEnvironment(t, orchestration.LocalEnvironmentName)
	previousRuntime := runtimeContext
	t.Cleanup(func() { runtimeContext = previousRuntime })

	for _, selection := range []string{
		resources.RuntimeContextNative,
		resources.RuntimeContextNix,
		resources.RuntimeContextContainer,
		resources.RuntimeContextFree,
	} {
		t.Run(selection, func(t *testing.T) {
			t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
			t.Setenv(manager.AgentSourceEnv, "local")
			t.Setenv(recoveryscope.EnvironmentVariable, "")
			runtimeContext = selection
			workspace, module, service := loadRecoveryFixture(t, "")

			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			flow, err := initRunService(ctx, workspace, module, service)
			require.Error(t, err, "the fixture must not reach a running agent")
			require.NotNil(t, flow)

			// An agent resolves the marker with its own Core, so what must match
			// is the identity it will acknowledge, not the raw variable.
			projected := recoveryscope.Acknowledgement()
			require.NotEmpty(t, projected)

			expected, err := dockerrun.NewContainerRecoveryScope(
				resources.CodeflyHomeDir(), workspace.Dir(), flow.Environment().NamingScope)
			require.NoError(t, err)
			require.NoError(t, dockerrun.SetContainerRecoveryScope(expected))
			require.Equal(t, recoveryscope.Acknowledgement(), projected)
		})
	}
}

// Preferences override the launch context per service, so a native run can hold
// a container-pinned service. Such a run projects a marker, which is what makes
// Runner.Init's acknowledgement guard reachable on a native launch at all — and
// every currently published agent answers with no acknowledgement, so it is
// rejected there rather than silently creating unlabeled containers.
func TestNativeRunWithAContainerPinnedServiceProjectsRecovery(t *testing.T) {
	useRunEnvironment(t, orchestration.LocalEnvironmentName)
	previousRuntime := runtimeContext
	t.Cleanup(func() { runtimeContext = previousRuntime })

	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	t.Setenv(manager.AgentSourceEnv, "local")
	t.Setenv(recoveryscope.EnvironmentVariable, "")
	runtimeContext = resources.RuntimeContextNative
	workspace, module, service := loadRecoveryFixture(t,
		"runtime:\n    by-service:\n        api: container\n")

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	flow, err := initRunService(ctx, workspace, module, service)
	require.Error(t, err, "the fixture must not reach a running agent")
	require.NotNil(t, flow)
	require.Contains(t, flow.SelectableRuntimeContexts(), resources.RuntimeContextContainer)
	require.NotEmpty(t, recoveryscope.Acknowledgement())
}
