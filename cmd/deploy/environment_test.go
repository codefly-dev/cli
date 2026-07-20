package deploy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestInitDeployServiceFailsFastOnUndeclaredEnvironment(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	files := map[string]string{
		"workspace.codefly.yaml": `name: deploy-env
layout: modules
modules:
    - name: web
`,
		"modules/web/module.codefly.yaml": `kind: module
name: web
project: deploy-env
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

	previous := envInput
	t.Cleanup(func() { envInput = previous })
	envInput = "production"

	flow, err := initDeployService(ctx, workspace, module, service, true)
	require.Nil(t, flow)
	require.Error(t, err)
	require.Contains(t, err.Error(), `"production"`)
	require.Contains(t, err.Error(), "deploy-env")
}
