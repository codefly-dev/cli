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

// derivedRunnableWorkspace is a workspace whose module backend serves one
// HTTP endpoint with derived runnable operations, as `generate contracts` and
// `generate runnables` leave it, with the current directory set to it.
func derivedRunnableWorkspace(t *testing.T) string {
	t.Helper()
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
		"modules/backend/module.contract.codefly.yaml": `schema: codefly/module-contract/v1
principal: releases
namespaces: []
queues: []
scope_ceilings: []
bindings: []
destinations:
  - id: http
    service: api
    endpoint: http
    kind: module
`,
		"modules/backend/services/api/openapi/api.swagger.json": string(artifact),
	})
	t.Chdir(dir)
	require.NoError(t, generate.ContractsCmd.RunE(generate.ContractsCmd, []string{"backend"}))
	require.NoError(t, generate.RunnablesCmd.RunE(generate.RunnablesCmd, []string{"backend"}))
	return dir
}

func TestDoctorUsesEffectiveRunnableValueAndPreservesAuthoredFile(t *testing.T) {
	dir := derivedRunnableWorkspace(t)
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

// A run under a naming scope resolves other addresses, so its bindings are
// generated in that scope. Doctor checks them in the scope it is told, as
// `codefly run --naming-scope` and the generator read it, and in the
// environment's own when it is told none.
func TestDoctorChecksRunnableBindingsInTheNamedScope(t *testing.T) {
	dir := derivedRunnableWorkspace(t)
	flags := generate.RunnableBindingsCmd.Flags()
	require.NoError(t, flags.Set("naming-scope", "gate-x"))
	t.Cleanup(func() {
		require.NoError(t, flags.Set("naming-scope", ""))
		flags.Lookup("naming-scope").Changed = false
	})
	require.NoError(t, generate.RunnableBindingsCmd.RunE(generate.RunnableBindingsCmd, nil))

	report := runReadiness(t, workspaceReadinessOptions{dir: dir, namingScope: new("gate-x")})
	require.Equal(t, readinessStatusReady, report.Status, reportJSON(t, report))

	for remedy, opts := range map[string]workspaceReadinessOptions{
		"run `codefly generate runnable-bindings --env local`":                       {dir: dir},
		"run `codefly generate runnable-bindings --env local --naming-scope gate-y`": {dir: dir, namingScope: new("gate-y")},
	} {
		t.Run(remedy, func(t *testing.T) {
			diagnostic := requireCode(t, runReadiness(t, opts), codeConfigurationInvalid, checkStatusFail)
			require.Contains(t, diagnostic.Message, "out of date")
			require.Equal(t, remedy, diagnostic.Remediation)
		})
	}
}
