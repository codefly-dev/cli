package run

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// A composition root must survive the same selected-environment exclusions
// that orchestration applies when it builds the dependency graph.
func TestCompositionRootsRespectSelectedEnvironmentExclusions(t *testing.T) {
	oldEnv, oldProfile, oldExcluded := environmentName, profile, excludeDependencies
	t.Cleanup(func() { environmentName, profile, excludeDependencies = oldEnv, oldProfile, oldExcluded })
	profile = "small"
	excludeDependencies = []string{"host/admin", "worker/cli-only"}
	root := t.TempDir()
	workspaceYAML := `name: roots
layout: modules
modules:
  - name: host
  - name: worker
run-profiles:
  small:
    exclude-dependencies: [worker/profile-only]
environments:
  - name: local
    host:
      coordinate: example/local/cell
      component: host
      domain: example
      audience: host
      trust_domain: cluster.example
      delivery: host/web/rest
    ingress:
      - name: web
        service: web
        endpoint: rest
        hosts: [web.example.test]
      - name: admin
        service: admin
        endpoint: rest
        hosts: [admin.example.test]
    exclude-services:
      - service: worker/witness
  - name: staging
`
	for module, names := range map[string][]string{
		"host":   {"web", "admin"},
		"worker": {"api", "witness", "cli-only", "profile-only"},
	} {
		dir := filepath.Join(root, "modules", module)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		doc := "kind: module\nname: " + module + "\nservices:\n"
		for _, name := range names {
			doc += "  - name: " + name + "\n"
			serviceDir := filepath.Join(dir, "services", name)
			require.NoError(t, os.MkdirAll(serviceDir, 0o755))
			service := fmt.Sprintf("kind: service\nname: %s\nmodule: %s\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.ai\nendpoints:\n  - name: rest\n    api: rest\n    visibility: public\n    exposure: none\n", name, module)
			require.NoError(t, os.WriteFile(filepath.Join(serviceDir, "service.codefly.yaml"), []byte(service), 0o600))
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, "module.codefly.yaml"), []byte(doc), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "workspace.codefly.yaml"), []byte(workspaceYAML), 0o600))
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), root)
	require.NoError(t, err)
	for _, tc := range []struct {
		env  string
		want []string
	}{
		{"local", []string{"host/web", "worker/api"}},
		{"staging", []string{"host/web", "worker/api", "worker/witness"}},
	} {
		t.Run(tc.env, func(t *testing.T) {
			environmentName = tc.env
			roots, err := CompositionRoots(t.Context(), workspace)
			require.NoError(t, err)
			env, err := orchestration.SelectEnvironment(workspace, tc.env)
			require.NoError(t, err)
			require.NoError(t, environments.ValidateEnvironmentExclusions(t.Context(), workspace, env))
			graph, err := architecture.NewServiceDependencies(t.Context(), workspace, architecture.ExcludeServices(env.ExcludedServiceUniques()...))
			require.NoError(t, err)
			for _, root := range roots {
				_, err = graph.OrderTo(t.Context(), root)
				if err != nil {
					t.Errorf("selected composition root %s cannot be ordered: %v", root, err)
				}
			}
			require.Equal(t, tc.want, roots)
		})
	}
}
