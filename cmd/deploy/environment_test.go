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

func TestDirectApplyRequestedTreatsDryRunAndRenderOnlyAsNoMutation(t *testing.T) {
	previousDryRun := dryRun
	previousRenderOnly := renderOnly
	t.Cleanup(func() {
		dryRun = previousDryRun
		renderOnly = previousRenderOnly
	})

	for _, test := range []struct {
		name       string
		dryRun     bool
		renderOnly bool
		want       bool
	}{
		{name: "default", want: true},
		{name: "dry run", dryRun: true},
		{name: "render only", renderOnly: true},
		{name: "both", dryRun: true, renderOnly: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dryRun = test.dryRun
			renderOnly = test.renderOnly
			require.Equal(t, test.want, directApplyRequested())
		})
	}
}

func TestInitDeployServiceRejectsRemoteDirectApplyBeforeStartingFlow(t *testing.T) {
	previousEnv := envInput
	previousDryRun := dryRun
	previousRenderOnly := renderOnly
	t.Cleanup(func() {
		envInput = previousEnv
		dryRun = previousDryRun
		renderOnly = previousRenderOnly
	})
	envInput = "production"
	dryRun = false
	renderOnly = false
	workspace := &resources.Workspace{
		Name: "deploy-env",
		Environments: []*resources.Environment{{
			Name: "production",
			Cluster: &resources.EnvironmentCluster{
				Kind:       "eks",
				Kubeconfig: "/does/not/exist",
				Context:    "k3d-production",
			},
		}},
	}
	service := &resources.Service{Name: "gateway"}
	service.WithModule("web")

	flow, err := initDeployService(
		context.Background(),
		workspace,
		&resources.Module{Name: "web"},
		service,
		true,
	)

	require.Nil(t, flow)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exact local k3d target")
	require.Contains(t, err.Error(), "--render-only")
}
