package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// Keep real flow configuration resolution in front of the fake agent boundary:
// projection-only tests cannot detect a missing group stopping the build first.
func installConfigurationResolvingAgents(t *testing.T) {
	t.Helper()
	previous := serviceFlow
	t.Cleanup(func() { serviceFlow = previous })
	serviceFlow = func(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service,
		env *environments.Environment, build renderBuild, sink orchestration.OutputSink,
		destination func(*resources.Module, *resources.Service) string,
		record func(map[string]*builderv0.DeploymentOutput), recordServices func(map[string]*resources.Service),
		recordEndpoints func(map[string]map[string]string),
		_ func(map[string]map[string]string),
		recordConfigurations func(map[string]*basev0.Configuration, map[string][]string),
		recordPorts func(map[string]map[string]uint32),
		_ func(map[string]string),
	) error {
		flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.SnapshotMode)
		if err != nil {
			return err
		}
		flow.WithRuntimeContext(resources.RuntimeContextContainer)
		if err = flow.ConfigurationManager.Load(ctx, env.Runtime()); err != nil {
			return err
		}
		configurations, err := flow.WorkspaceConfigurationsFor(ctx, service)
		if err != nil {
			return err
		}
		require.Empty(t, configurations, "the render owns the group; it needs no manager entry")
		writeConsumerTree(t, destination(module, service), env.Name, env.ModuleNamespace(workspace, module.Name), service.Name, "database.example:5432")
		unique := resources.ServiceUnique(module.Name, service.Name)
		if record != nil {
			record(map[string]*builderv0.DeploymentOutput{unique: fakeRestrictedOutput()})
		}
		if recordServices != nil {
			recordServices(map[string]*resources.Service{unique: service})
		}
		return nil
	}
}

// The render supplies module-authority per workload, with no configuration
// file and on any cluster: the principal is the mesh's, which every cell has,
// so nothing about the group is local to a k3d environment any more.
func TestRenderSuppliesModuleAuthorityWithoutConfigurationFiles(t *testing.T) {
	for _, target := range []string{"module", "service"} {
		for _, kind := range []string{"k3d", "eks"} {
			t.Run(target+"/"+kind, func(t *testing.T) {
				workspace, env := hostComposition(t)
				env.Cluster = &environments.EnvironmentCluster{Kind: kind, Context: kind + "-example"}
				servicePath := filepath.Join(workspace.Dir(), "modules/app/services/api", resources.ServiceConfigurationName)
				require.NoError(t, os.WriteFile(servicePath, []byte(devServiceYAML("api")+"workspace-configuration-dependencies:\n  - module-authority\n"), 0o600))
				var err error
				workspace, err = resources.LoadWorkspaceFromDir(t.Context(), workspace.Dir())
				require.NoError(t, err)
				module, err := workspace.LoadModuleFromName(t.Context(), "app")
				require.NoError(t, err)
				service, err := module.LoadServiceFromName(t.Context(), "api")
				require.NoError(t, err)
				env.Host.Delivery = "edge/gateway/http"
				env.Registry = &environments.EnvironmentRegistry{URL: "registry.example.test/example"}
				env.ServiceSecrets = storeEnvironment().ServiceSecrets
				installConfigurationResolvingAgents(t)
				var result RenderResult
				unit := filepath.Join("services", "api")
				if target == "module" {
					result, err = RenderModule(t.Context(), workspace, module, env, "example", nil)
				} else {
					result, err = RenderService(t.Context(), workspace, module, service, env, "example", false, nil)
					unit = filepath.Join("modules", "app", unit)
				}
				require.NoError(t, err)
				rendered := buildOverlay(t, filepath.Join(result.Path, unit), env.Name)
				variables := containerEnvironment(t, rendered)
				data := configMapData(t, rendered)
				require.Equal(t, "spiffe://local.example/ns/example-app/sa/api", data[authorityPrefix+"PRINCIPAL"])
				require.Equal(t, env.Host.ModuleAudience, data[authorityPrefix+"AUDIENCE"])
				require.Equal(t, env.Host.Audience, data[authorityPrefix+"PROJECTION_AUDIENCE"])
				require.Equal(t, map[string]any{"secretKeyRef": map[string]any{"name": "secret-api", "key": storedSecretKey, "optional": false}}, variables[authorityToken]["valueFrom"])
				require.NotContains(t, variables[authorityToken], "value")
				for key := range variables {
					require.NotContains(t, key, "WORKLOAD_IDENTITY")
				}
				require.NoDirExists(t, filepath.Join(workspace.Dir(), "configurations"))
			})
		}
	}
}
