package ci

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestCIDeployRejectsInvalidRoleBeforeStartingAgents(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"workspace.codefly.yaml":                        "name: preflight\nlayout: modules\nmodules:\n  - name: app\n",
		"modules/app/module.codefly.yaml":               "kind: module\nname: app\nservices:\n  - name: api\n",
		"modules/app/services/api/service.codefly.yaml": "kind: service\nname: api\nversion: 0.0.0\nmodule: app\n",
		"modules/app/roles.codefly.yaml": `schema: codefly/solution-roles/v1
scope_ceilings:
  - {resource_kind: app.entry, actions: [read]}
roles:
  - name: installer
    scopes:
      - {resource_kind: app.entry, actions: [typo]}
`,
	}
	for path, data := range files {
		full := filepath.Join(root, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0755))
		require.NoError(t, os.WriteFile(full, []byte(data), 0644))
	}
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), root)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(t.Context(), "app")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(t.Context(), "api")
	require.NoError(t, err)
	previous := envInput
	envInput = "local"
	t.Cleanup(func() { envInput = previous })
	flow, _, err := initDeployService(t.Context(), workspace, module, service)
	require.Nil(t, flow)
	require.ErrorContains(t, err, "app.entry:typo")
}
