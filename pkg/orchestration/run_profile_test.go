package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func writeRunProfileWorkspace(t *testing.T, workspaceYAML string) *resources.Workspace {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"workspace.codefly.yaml": workspaceYAML,
		"modules/app/module.codefly.yaml": `kind: module
name: app
services:
    - name: api
    - name: local
    - name: managed
`,
		"modules/app/services/api/service.codefly.yaml": `kind: service
name: api
version: 0.0.0
module: app
agent:
    kind: codefly:service
    name: go-grpc
    version: 0.1.27
    publisher: codefly.dev
workspace-configuration-dependencies:
    - local-auth
    - managed-auth
`,
		"modules/app/services/local/service.codefly.yaml": `kind: service
name: local
version: 0.0.0
module: app
agent:
    kind: codefly:service
    name: redis
    version: 0.0.74
    publisher: codefly.dev
`,
		"modules/app/services/managed/service.codefly.yaml": `kind: service
name: managed
version: 0.0.0
module: app
agent:
    kind: codefly:service
    name: redis
    version: 0.0.74
    publisher: codefly.dev
`,
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	require.NoError(t, err)
	return workspace
}

func TestResolveRunProfileComposesCanonicalExclusions(t *testing.T) {
	workspace := writeRunProfileWorkspace(t, `name: profiles
layout: modules
modules:
    - name: app
run-profiles:
    local:
        exclude-dependencies:
            - managed
        exclude-workspace-configurations:
            - managed-auth
    saas: {}
`)

	resolved, err := ResolveRunProfile(context.Background(), workspace, "local", []string{"app/local, managed"})
	require.NoError(t, err)
	require.Equal(t, []string{"app/local", "app/managed"}, resolved.ExcludedDependencies)
	require.Equal(t, []string{"managed-auth"}, resolved.ExcludedWorkspaceConfigurations)

	saas, err := ResolveRunProfile(context.Background(), workspace, "saas", nil)
	require.NoError(t, err)
	require.Empty(t, saas.ExcludedDependencies)
	require.Empty(t, saas.ExcludedWorkspaceConfigurations)
}

func TestResolveRunProfileRejectsUnknownReferences(t *testing.T) {
	tests := []struct {
		name          string
		workspaceYAML string
		profile       string
		explicit      []string
		want          string
	}{
		{
			name: "profile",
			workspaceYAML: `name: profiles
layout: modules
modules:
    - name: app
`,
			profile: "missing",
			want:    `unknown run profile "missing"`,
		},
		{
			name: "profile service",
			workspaceYAML: `name: profiles
layout: modules
modules:
    - name: app
run-profiles:
    local:
        exclude-dependencies:
            - app/missing
`,
			profile: "local",
			want:    `resolve excluded dependency "app/missing"`,
		},
		{
			name: "explicit service",
			workspaceYAML: `name: profiles
layout: modules
modules:
    - name: app
`,
			explicit: []string{"app/missing"},
			want:     `resolve excluded dependency "app/missing"`,
		},
		{
			name: "workspace configuration",
			workspaceYAML: `name: profiles
layout: modules
modules:
    - name: app
run-profiles:
    local:
        exclude-workspace-configurations:
            - missing-auth
`,
			profile: "local",
			want:    `run profile "local" excludes unknown workspace configuration "missing-auth"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := writeRunProfileWorkspace(t, tt.workspaceYAML)
			_, err := ResolveRunProfile(context.Background(), workspace, tt.profile, tt.explicit)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestRunProfileCannotApplyToDeploymentFlow(t *testing.T) {
	flow := &Flow{world: &World{Mode: DeployMode}}
	err := flow.WithRunProfile(ResolvedRunProfile{
		ExcludedDependencies:            []string{"app/managed"},
		ExcludedWorkspaceConfigurations: []string{"managed-auth"},
	})
	require.ErrorContains(t, err, "only be applied to run flows")
	require.Empty(t, flow.excludedDependencyServices)
	require.Empty(t, flow.world.excludedWorkspaceConfigurations)
}
