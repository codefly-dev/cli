package orchestration

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func selectedSolutionFlow(t *testing.T, edit func(string)) *Flow {
	t.Helper()
	world, workspace := hostRoutesWorld(t, RunMode, edit)
	module, err := workspace.LoadModuleFromName(t.Context(), "wiki")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(t.Context(), "backend")
	require.NoError(t, err)
	return &Flow{workspace: workspace, originModule: module, originService: service, moduleSeeds: []string{"wiki"}, world: world}
}

func TestSelectedSolutionStartsConsumedAPIsWithoutMeshDependencies(t *testing.T) {
	flow := selectedSolutionFlow(t, nil)
	require.Empty(t, flow.originService.ServiceDependencies)
	require.NoError(t, flow.includeConsumedRoots(t.Context()))
	selected, err := flow.runClosure(t.Context())
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"host/gateway", "docs/documents"}, uniques(selected))
	require.ElementsMatch(t, []string{"wiki", "host", "docs"}, moduleNames(flow.graphWorkspace))
	require.Empty(t, flow.originService.ServiceDependencies, "membership must not authorize direct mesh transport")
	depends, err := flow.world.Dependencies.DependsOn("wiki/backend", "docs/documents")
	require.NoError(t, err)
	require.False(t, depends)
	require.NoError(t, flow.includeConsumedRoots(t.Context()))
	require.Len(t, flow.rootUniques(), 3, "recomputation must not duplicate roots")
	require.Contains(t, flow.rootBeginActions(), Action{Type: RuntimeBegin, Service: "docs/documents"},
		"the producer must enter the existing lifecycle, not merely appear in the module set")
}

func TestConsumedProducerKeepsItsTransitiveDependencies(t *testing.T) {
	flow := selectedSolutionFlow(t, func(dir string) {
		path := filepath.Join(dir, "modules/docs/services/documents/service.codefly.yaml")
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		data = append(data, []byte("service-dependencies:\n    - name: store\n    - name: gateway\n      module: host\n      endpoints:\n        - name: rest\n")...)
		require.NoError(t, os.WriteFile(path, data, 0o600))
	})
	require.NoError(t, flow.includeConsumedRoots(t.Context()))
	selected, err := flow.runClosure(t.Context())
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"host/gateway", "docs/store", "docs/documents"}, uniques(selected),
		"shared dependencies run once and consumed services keep their own dependencies")
	require.Empty(t, flow.originService.ServiceDependencies)
}

func TestConsumedSelectionDoesNotGrantGatewayPrivateEndpointAccess(t *testing.T) {
	flow := selectedSolutionFlow(t, func(dir string) {
		writeWikiConsumes(t, dir, "    - id: docs\n      protocol: http\n      module: docs\n      service: documents\n      endpoint: admin\n      as: docs\n")
	})
	require.NoError(t, flow.includeConsumedRoots(t.Context()))
	gateway := parityService(t, flow.workspace, "host", "gateway")
	producer := parityService(t, flow.workspace, "docs", "documents")
	identity, err := producer.Identity()
	require.NoError(t, err)
	endpoints, err := producer.LoadEndpoints(t.Context())
	require.NoError(t, err)
	require.NoError(t, flow.world.SharedState.RecordEndpoints(t.Context(), identity, endpoints))
	carriers, problems, err := flow.world.hostRouteCarriers(t.Context(), gateway, resources.NewContainerNetworkAccess())
	require.NoError(t, err)
	require.ErrorContains(t, errors.Join(problems...), "visibility does not admit")
	require.Empty(t, carriers, "a denied endpoint must not be projected to the gateway")
}

func TestUnselectedSolutionCannotStartServicesOrRejectSelectedRoutes(t *testing.T) {
	flow := selectedSolutionFlow(t, func(dir string) {
		require.NoError(t, os.CopyFS(filepath.Join(dir, "modules/other"), os.DirFS(filepath.Join(dir, "modules/wiki"))))
		for _, path := range []string{"module.codefly.yaml", "services/backend/service.codefly.yaml", "solution.codefly.yaml"} {
			name := filepath.Join(dir, "modules/other", path)
			data, err := os.ReadFile(name)
			require.NoError(t, err)
			text := strings.ReplaceAll(string(data), "wiki", "other")
			// Both consumes are legal individually but disagree on the shared
			// module route. A workspace render must still reject this conflict.
			text = strings.ReplaceAll(text, "endpoint: web", "endpoint: rest")
			require.NoError(t, os.WriteFile(name, []byte(text), 0o600))
		}
		name := filepath.Join(dir, "workspace.codefly.yaml")
		file, err := os.OpenFile(name, os.O_APPEND|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		_, err = file.WriteString("    - name: other\n")
		require.NoError(t, err)
		require.NoError(t, file.Close())
	})
	require.NoError(t, flow.includeConsumedRoots(t.Context()))
	selected, err := flow.runClosure(t.Context())
	require.NoError(t, err)
	require.NotContains(t, uniques(selected), "other/backend")
	require.NoError(t, flow.scopeDependenciesToRun(t.Context(), uniques(selected), nil))
	flow.world.setRunProducers(uniques(selected), flow.originService)
	routes, err := flow.world.hostRoutes(t.Context(), "host")
	require.NoError(t, err)
	require.NotEmpty(t, routes)
	_, err = HostRouteTargets(t.Context(), flow.workspace, "host")
	require.ErrorContains(t, err, "different endpoints")
}

func TestSelectedSolutionRefusesMissingConsumedEndpoint(t *testing.T) {
	flow := selectedSolutionFlow(t, func(dir string) {
		name := filepath.Join(dir, "modules/wiki/solution.codefly.yaml")
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(name, []byte(strings.ReplaceAll(string(data), "endpoint: web", "endpoint: absent")), 0o600))
	})
	require.ErrorContains(t, flow.includeConsumedRoots(t.Context()), "solution wiki consumes undeclared endpoint docs/documents/absent")
}

func TestConsumedRootsAreRecomputedWhenSolutionIsWithdrawn(t *testing.T) {
	flow := selectedSolutionFlow(t, nil)
	// Begin with the host origin and Wiki as the authored co-root.
	module, err := flow.workspace.LoadModuleFromName(t.Context(), "host")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(t.Context(), "gateway")
	require.NoError(t, err)
	flow.originModule, flow.originService = module, service
	flow.coRoots = []string{"wiki/backend"}
	require.NoError(t, flow.includeConsumedRoots(t.Context()))
	require.Contains(t, flow.rootUniques(), "docs/documents")
	flow.coRoots = nil
	require.NoError(t, flow.includeConsumedRoots(t.Context()))
	require.Equal(t, []string{resources.ServiceUnique("host", "gateway")}, flow.rootUniques())
	require.Equal(t, []string{"host"}, moduleNames(flow.graphWorkspace))
}

func TestDeclaredConfigurationAddsAProducerButRootOnlyConfigurationDoesNot(t *testing.T) {
	for _, declared := range []bool{false, true} {
		t.Run(map[bool]string{false: "root only", true: "declared"}[declared], func(t *testing.T) {
			flow := selectedSolutionFlow(t, func(dir string) {
				require.NoError(t, os.Remove(filepath.Join(dir, "modules/wiki/solution.codefly.yaml")))
				if declared {
					path := filepath.Join(dir, "modules/wiki/services/backend/service.codefly.yaml")
					file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
					require.NoError(t, err)
					_, err = file.WriteString("workspace-configuration-dependencies:\n    - provider\n")
					require.NoError(t, err)
					require.NoError(t, file.Close())
				}
			})
			provided := &configurations.WorkspaceConfigurations{Infos: workspaceConfiguration("provider", "endpoint", "${endpoint:docs/documents/web}").Infos}
			flow.providedWorkspaceConfigurations = provided
			flow.configurationReferences = configurationReferenceOptionFrom(provided)
			require.NoError(t, flow.includeConsumedRoots(t.Context()))
			selected, err := flow.runClosure(t.Context())
			require.NoError(t, err)
			if declared {
				require.Equal(t, []string{"docs/documents"}, uniques(selected))
			} else {
				require.Empty(t, selected)
			}
		})
	}
}

func TestRejectedConsumedEndpointDoesNotPoisonAdmissionRetry(t *testing.T) {
	flow := selectedSolutionFlow(t, nil)
	module, err := flow.workspace.LoadModuleFromName(t.Context(), "host")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(t.Context(), "gateway")
	require.NoError(t, err)
	flow.originModule, flow.originService = module, service
	flow.moduleSeeds = []string{"host"}
	require.NoError(t, flow.includeConsumedRoots(t.Context()))
	path := filepath.Join(flow.workspace.Dir(), "modules/wiki/solution.codefly.yaml")
	valid, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(strings.ReplaceAll(string(valid), "endpoint: web", "endpoint: absent")), 0o600))
	_, err = flow.admitRoots(t.Context(), flow.workspace, []string{"wiki/backend"})
	require.ErrorContains(t, err, "undeclared endpoint")
	require.Equal(t, []string{"host/gateway"}, flow.rootUniques())
	require.NoError(t, os.WriteFile(path, valid, 0o600))
	added, err := flow.admitRoots(t.Context(), flow.workspace, []string{"wiki/backend"})
	require.NoError(t, err)
	require.Equal(t, []string{"wiki/backend"}, added)
	require.NoError(t, flow.includeConsumedRoots(t.Context()))
	require.Contains(t, flow.rootUniques(), "docs/documents")
}

func TestConsumedEndpointReadinessCannotBeSatisfiedByASiblingMapping(t *testing.T) {
	flow := selectedSolutionFlow(t, nil)
	require.NoError(t, flow.includeConsumedRoots(t.Context()))
	flow.SharedState = &StateManager{networkMappings: map[string][]*basev0.NetworkMapping{
		"docs/documents": {testHTTPMapping("rest", "http://127.0.0.1:1")},
	}}
	selected, err := flow.runClosure(t.Context())
	require.NoError(t, err)
	consumers, err := flow.dependencyConsumers(selected)
	require.NoError(t, err)
	requirements, err := flow.endpointRequirements("docs/documents", consumers["docs/documents"])
	require.NoError(t, err)
	require.Len(t, requirements, 1)
	require.Equal(t, PredicateMapping, requirements[0].predicate)
	require.Equal(t, "web", requirements[0].endpoint)
	require.Contains(t, requirements[0].reason, "no network mapping")
}
