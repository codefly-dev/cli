package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/cmd/deploy"
	"github.com/codefly-dev/cli/cmd/generate"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestDoctorUsesEffectiveRunnableValueAndPreservesAuthoredFile(t *testing.T) {
	artifact, err := os.ReadFile("generate/testdata/solution-runnables/api.swagger.json")
	require.NoError(t, err)
	dir := singleServiceWorkspace(t, testWorkspaceYAMLLocalDeclared, []string{generate.RunnableBindingsGroup}, map[string]string{
		"modules/backend/services/api/service.codefly.yaml": strings.NewReplacer("runtime::service", "codefly:service", "codefly.ai", "codefly.dev").Replace(testServiceYAML("api", generate.RunnableBindingsGroup)) + `endpoints:
    - name: http
      api: http
      visibility: public
      exposure: none
`,
		"modules/backend/module.codefly.yaml": testModuleYAML("api") + `interface:
    endpoints:
        - service: api
          endpoint: http
          visibility: public
`,
		"modules/backend/services/api/openapi/api.swagger.json": string(artifact),
	})
	t.Chdir(dir)
	require.NoError(t, generate.ContractsCmd.RunE(generate.ContractsCmd, []string{"backend"}))
	require.NoError(t, generate.RunnablesCmd.RunE(generate.RunnablesCmd, []string{"backend"}))
	require.NoError(t, generate.RunnableBindingsCmd.RunE(generate.RunnableBindingsCmd, nil))
	path := filepath.Join(dir, "configurations/local/runnable-bindings.env")
	fresh, err := os.ReadFile(path)
	require.NoError(t, err)
	var key, value string
	for _, line := range strings.Split(string(fresh), "\n") {
		if !strings.HasPrefix(line, "#") && strings.Contains(line, "=") {
			key, value, _ = strings.Cut(line, "=")
			break
		}
	}
	require.NotEmpty(t, key)
	for _, validOverride := range []bool{false, true} {
		t.Run(map[bool]string{false: "stale override", true: "valid replacement"}[validOverride], func(t *testing.T) {
			authored, override := fresh, "{}"
			if validOverride {
				authored = []byte(strings.Replace(string(fresh), key+"="+value, key+"={}", 1))
				override = value
			}
			require.NoError(t, os.WriteFile(path, authored, 0o600))
			encoded, err := resources.EncodeWorkspaceConfigurationOverrides([]resources.WorkspaceConfigurationOverride{{Name: generate.RunnableBindingsGroup, Key: key, Value: override}})
			require.NoError(t, err)
			t.Setenv(resources.WorkspaceConfigurationOverridesEnvironment, encoded)
			report := runReadiness(t, workspaceReadinessOptions{dir: dir, module: "backend"})
			if validOverride {
				require.Equal(t, readinessStatusReady, report.Status, reportJSON(t, report))
				require.NoError(t, deploy.WorkspaceReadiness(context.Background(), "local", "backend"))
			} else {
				requireCode(t, report, codeConfigurationInvalid, checkStatusFail)
				require.ErrorContains(t, deploy.WorkspaceReadiness(context.Background(), "local", "backend"), "out of date")
			}
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, authored, after)
		})
	}
}
