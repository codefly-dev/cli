package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
)

func TestContainerRecoveryRequiresAgentAcknowledgementBeforeDockerInit(t *testing.T) {
	t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, "")
	scope, err := dockerrun.NewContainerRecoveryScope(t.TempDir(), t.TempDir(), "test")
	require.NoError(t, err)
	require.NoError(t, dockerrun.SetContainerRecoveryScope(scope))
	for _, tc := range []struct {
		name, runtime, ack string
		reject             bool
	}{
		{"legacy Docker agent", resources.RuntimeContextContainer, "", true},
		{"legacy free agent", resources.RuntimeContextFree, "", true},
		{"wrong scope", resources.RuntimeContextContainer, "foreign", true},
		{"acknowledged", resources.RuntimeContextContainer, dockerrun.InheritedContainerRecoveryScope(), false},
		{"native", resources.RuntimeContextNative, "", false},
		{"nix", resources.RuntimeContextNix, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &Runner{runtimeContext: tc.runtime, instance: &services.Instance{Identity: &resources.ServiceIdentity{Name: "db", Module: "infra"}, ContainerRecoveryScope: tc.ack}}
			if tc.reject {
				// No Runtime client or World is installed: the real Init must
				// return before configuration, port allocation or any agent RPC.
				_, err := runner.Init(t.Context())
				require.ErrorContains(t, err, "did not acknowledge this run's container recovery scope")
			} else {
				require.NoError(t, runner.validateContainerRecovery())
			}
		})
	}
	t.Run("invalid inherited identity", func(t *testing.T) {
		t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, "malformed")
		runner := &Runner{runtimeContext: resources.RuntimeContextContainer, instance: &services.Instance{Identity: &resources.ServiceIdentity{Name: "db", Module: "infra"}}}
		_, err := runner.Init(t.Context())
		require.ErrorContains(t, err, "invalid container recovery identity")
	})
}

// newProjectionWorkspaceFlow builds a real flow whose origin is excluded and
// whose dependency graph is cut, so InitManagers runs its whole sequence
// without spawning an agent process.
func newProjectionWorkspaceFlow(t *testing.T, mode Mode) (*Flow, *resources.Workspace) {
	t.Helper()
	ctx := context.Background()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": `name: recovery
layout: modules
modules:
    - name: web
environments:
    - name: local
      naming-scope: from-yaml
`,
		"modules/web/module.codefly.yaml": `kind: module
name: web
project: recovery
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
`,
	})
	module, err := workspace.LoadModuleFromName(ctx, "web")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "gateway")
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	flow, err := NewFlow(ctx, workspace, module, service, env, mode)
	require.NoError(t, err)
	flow.WithStandAlone(true)
	flow.WithExcludeRoot(true)
	return flow, workspace
}

// Ownership is projected by initializing managers at all, so every entry point
// that spawns agents through a flow — build, test, ci, deploy, sync,
// validation, gitops and the control plane, not only run — labels the
// containers its agents create.
func TestEveryFlowProjectsContainerRecoveryBeforeSpawningAgents(t *testing.T) {
	for _, mode := range []Mode{RunMode, BuildMode, TestMode, SyncMode, DeployMode, SnapshotMode} {
		t.Run(string(mode), func(t *testing.T) {
			t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
			t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, "")
			flow, workspace := newProjectionWorkspaceFlow(t, mode)

			require.NoError(t, flow.InitManagers(context.Background()))

			// Read the environment before asking the flow for its scope:
			// ContainerRecoveryScope projects on demand, so consulting it first
			// would pass whether or not InitManagers ever projected.
			require.NotEmpty(t, dockerrun.InheritedContainerRecoveryScope())

			expected, err := dockerrun.NewContainerRecoveryScope(resources.CodeflyHomeDir(), workspace.Dir(), "from-yaml")
			require.NoError(t, err)
			scope, err := flow.ContainerRecoveryScope()
			require.NoError(t, err)
			require.Equal(t, expected, scope)
		})
	}
}

// The sweep resolves ownership before InitManagers does, and an agent already
// holding the inherited marker must keep acknowledging the same identity.
func TestContainerRecoveryScopeIsResolvedOnce(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, "")
	flow, _ := newProjectionWorkspaceFlow(t, RunMode)

	first, err := flow.ContainerRecoveryScope()
	require.NoError(t, err)
	marker := os.Getenv(dockerrun.ContainerRecoveryScopeEnvironment)
	require.NotEmpty(t, marker)

	require.NoError(t, flow.InitManagers(context.Background()))

	second, err := flow.ContainerRecoveryScope()
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, marker, os.Getenv(dockerrun.ContainerRecoveryScopeEnvironment))
}
