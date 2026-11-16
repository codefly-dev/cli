package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDoctorJudgesOnlyServicesSelectedByTheEnvironment(t *testing.T) {
	dir := writeTestWorkspace(t, map[string]string{
		"workspace.codefly.yaml": testWorkspaceYAML + `environments:
    - name: local
    - name: aurora
      exclude-services:
        - service: backend/local
`,
		"modules/backend/module.codefly.yaml":                 testModuleYAML("api", "local"),
		"modules/backend/services/api/service.codefly.yaml":   testServiceYAML("api", "active"),
		"modules/backend/services/local/service.codefly.yaml": testServiceYAML("local", "fixture-composition", "local-only"),
		"configurations/aurora/active.env":                    "VALUE=present\n",
		"configurations/local/active.env":                     "VALUE=present\n",
	})
	for _, module := range []string{"", "backend"} {
		report := runReadiness(t, workspaceReadinessOptions{dir: dir, env: "aurora", module: module})
		require.Equal(t, readinessStatusReady, report.Status, reportJSON(t, report))
		require.Contains(t, findCheck(report, "services").Message, "1 service(s)")
	}
	// The same service still imposes its ordinary requirements where included.
	// Fixture composition itself is supplied by the dedicated empty merger.
	report := runReadiness(t, workspaceReadinessOptions{dir: dir, env: "local"})
	requireCode(t, report, codeConfigurationMissing, checkStatusFail)
	// Selecting an excluded root explicitly must not claim it can run there.
	report = runReadiness(t, workspaceReadinessOptions{dir: dir, env: "aurora", service: "backend/local"})
	require.Equal(t, readinessStatusNotReady, report.Status)
	require.Contains(t, reportJSON(t, report), "excludes backend/local")
}
