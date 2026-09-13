package run

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestShouldSweepStaleContainersOnlyForDockerCapableSelections(t *testing.T) {
	tests := []struct {
		name    string
		runtime string
		want    bool
	}{
		{name: "free may resolve to Docker", runtime: resources.RuntimeContextFree, want: true},
		{name: "container requires Docker", runtime: resources.RuntimeContextContainer, want: true},
		{name: "native is Docker independent", runtime: resources.RuntimeContextNative},
		{name: "nix is Docker independent", runtime: resources.RuntimeContextNix},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldSweepStaleContainers(test.runtime); got != test.want {
				t.Fatalf("shouldSweepStaleContainers(%q) = %v, want %v", test.runtime, got, test.want)
			}
		})
	}
}

// containerRecoveryWorkspace writes a one-service workspace, optionally with a
// developer preferences file, and returns the flow `codefly run service` would
// build for it.
func containerRecoveryWorkspace(t *testing.T, preferences string) *orchestration.Flow {
	t.Helper()
	useRunEnvironment(t, orchestration.LocalEnvironmentName)
	ctx := context.Background()
	dir := t.TempDir()
	files := map[string]string{
		"workspace.codefly.yaml": `name: sweep
layout: modules
modules:
    - name: infra
environments:
    - name: local
      naming-scope: from-yaml
`,
		"modules/infra/module.codefly.yaml": `kind: module
name: infra
project: sweep
services:
    - name: postgres
`,
		"modules/infra/services/postgres/service.codefly.yaml": `kind: service
name: postgres
version: 0.0.0
module: infra
agent:
    kind: runtime::service
    name: postgres
    version: 0.0.131
    publisher: codefly.dev
`,
	}
	if preferences != "" {
		files[resources.UserPreferencesFile] = preferences
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	workspace, err := resources.LoadWorkspaceFromDir(ctx, dir)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "infra")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "postgres")
	require.NoError(t, err)

	previousContext, previousPorts := runtimeContext, temporaryPorts
	t.Cleanup(func() { runtimeContext, temporaryPorts = previousContext, previousPorts })
	runtimeContext = resources.RuntimeContextNative
	temporaryPorts = true

	flow, err := newRunFlow(ctx, workspace, module, service)
	require.NoError(t, err)
	return flow
}

// sweeps mirrors the decision initRunService makes before spawning any agent.
func sweeps(flow *orchestration.Flow) bool {
	return slices.ContainsFunc(flow.SelectableRuntimeContexts(), shouldSweepStaleContainers)
}

// A native run is Docker independent only while nothing in it selects Docker.
// Preferences override the launch context per service, so deciding from the
// launch flag alone skipped both the recovery marker and the ownership guard
// for a container-pinned service — which then created containers carrying no
// recovery label, silently unreapable.
func TestNativeRunStillSweepsWhenAPreferencePinsAContainerContext(t *testing.T) {
	t.Run("no preferences", func(t *testing.T) {
		if sweeps(containerRecoveryWorkspace(t, "")) {
			t.Fatal("a native run with no container-capable selection swept anyway")
		}
	})
	for _, test := range []struct {
		name, preferences string
	}{
		{"by-service", "runtime:\n    by-service:\n        postgres: container\n"},
		{"by-agent", "runtime:\n    by-agent:\n        postgres: container\n"},
		{"default", "runtime:\n    default: free\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !sweeps(containerRecoveryWorkspace(t, test.preferences)) {
				t.Fatal("a native run holding a container-capable preference did not project the recovery marker")
			}
		})
	}
}
