package orchestration

import (
	"fmt"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// Real manifests and root configuration files reproduce a single-root run.
// Neither configuration producer is reachable through a service dependency.
func configurationClosureFlow(t *testing.T, visibility string) *Flow {
	t.Helper()
	files := map[string]string{
		"workspace.codefly.yaml":             "name: example\nlayout: modules\nmodules:\n  - name: app\n  - name: provider\n  - name: catalog\n  - name: storage\n  - name: unused\n",
		"configurations/local/provider.env":  "endpoint=${endpoint:provider/api/http}\n",
		"configurations/local/catalog.env":   "endpoint=${endpoint:catalog/api/http}\n",
		"configurations/local/root-only.env": "endpoint=${endpoint:unused/api/http}\n",
	}
	for _, module := range []string{"app", "provider", "catalog", "storage", "unused"} {
		files[fmt.Sprintf("modules/%s/module.codefly.yaml", module)] = fmt.Sprintf("kind: module\nname: %s\nservices:\n  - name: api\n", module)
		endpointVisibility := "public"
		if module == "provider" {
			endpointVisibility = visibility
		}
		service := fmt.Sprintf("kind: service\nname: api\nversion: 0.0.0\nmodule: %s\nagent:\n  kind: runtime::service\n  name: go-grpc\n  version: 0.0.16\n  publisher: codefly.ai\nendpoints:\n  - name: http\n    api: http\n    visibility: %s\n", module, endpointVisibility)
		switch module {
		case "app":
			service += "workspace-configuration-dependencies:\n  - provider\n"
		case "provider":
			service += "workspace-configuration-dependencies:\n  - catalog\n"
		case "catalog":
			service += "service-dependencies:\n  - module: storage\n    name: api\n"
		}
		files[fmt.Sprintf("modules/%s/services/api/service.codefly.yaml", module)] = service
	}
	workspace := writeTempWorkspace(t, files)
	module, err := workspace.LoadModuleFromName(t.Context(), "app")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(t.Context(), "api")
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	flow, err := NewFlow(t.Context(), workspace, module, service, env, RunMode, WithRunModuleClosure("app"))
	require.NoError(t, err)
	return flow
}

func TestSingleRootConfigurationClosureIncludesTransitiveProducers(t *testing.T) {
	flow := configurationClosureFlow(t, "public")
	order, err := flow.runClosure(t.Context())
	require.NoError(t, err)
	var required []string
	for _, service := range order {
		required = append(required, service.Unique)
	}
	require.NoError(t, flow.checkConfigurationReferences(t.Context(), required), "preflight must resolve declared producers without adding run roots")
	require.Equal(t, []string{"storage/api", "catalog/api", "provider/api"}, required)
	require.ElementsMatch(t, []string{"app", "provider", "catalog", "storage"}, moduleNames(flow.graphWorkspace))
	require.Equal(t, []string{"app/api"}, flow.rootUniques())
	_, options := flow.dependencyGraphOptions()
	require.NoError(t, flow.scopeDependenciesToRun(t.Context(), required, options))
	require.NoError(t, flow.checkConfigurationReferences(t.Context(), required))
	for _, consumer := range []string{"app/api", "provider/api"} {
		refs := flow.world.Dependencies.ConfigurationReferenceDependencies(consumer)
		require.Len(t, refs, 1)
		require.Equal(t, "http", refs[0].Endpoints[0].Name, "the admitted endpoint still gates readiness")
	}
}

func TestSingleRootConfigurationClosurePreservesExclusionRefusal(t *testing.T) {
	flow := configurationClosureFlow(t, "public")
	require.NoError(t, flow.WithRunProfile(resources.RunProfile{ExcludeDependencies: []string{"provider/api"}}))
	_, options := flow.dependencyGraphOptions()
	require.NoError(t, flow.rebuildDependencyGraph(t.Context(), options))
	_, err := flow.world.Dependencies.ServiceFromUnique("provider/api")
	require.Error(t, err)
	require.ErrorContains(t, flow.checkConfigurationReferences(t.Context(), nil), "excluded")
}

func TestSingleRootConfigurationClosurePreservesVisibilityRefusal(t *testing.T) {
	flow := configurationClosureFlow(t, "internal")
	err := flow.checkConfigurationReferences(t.Context(), nil)
	require.ErrorContains(t, err, "does not permit module", "loading a producer does not export its endpoint")
	require.NotContains(t, err.Error(), "not a service of this workspace")
}

func TestSingleRootConfigurationClosureUsesInvocationSelection(t *testing.T) {
	encoded, err := resources.EncodeWorkspaceConfigurationOverrides([]resources.WorkspaceConfigurationOverride{
		{Name: "provider", Key: "endpoint", Value: "${endpoint:unused/api/http}"},
	})
	require.NoError(t, err)
	t.Setenv(resources.WorkspaceConfigurationOverridesEnvironment, encoded)
	flow := configurationClosureFlow(t, "public")
	order, err := flow.runClosure(t.Context())
	require.NoError(t, err)
	require.Len(t, order, 1)
	require.Equal(t, "unused/api", order[0].Unique)
	require.ElementsMatch(t, []string{"app", "unused"}, moduleNames(flow.graphWorkspace))
	require.NoError(t, flow.checkConfigurationReferences(t.Context(), []string{"unused/api"}))
}
