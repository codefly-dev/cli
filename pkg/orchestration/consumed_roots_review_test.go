package orchestration

import (
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConsumedClosureNonEntryRunDoesNotSelectSolutionRoutes(t *testing.T) {
	flow := selectedSolutionFlow(t, func(dir string) {
		path := filepath.Join(dir, "modules/wiki/module.codefly.yaml")
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, append(data, []byte("    - name: worker\n")...), 0600))
		path = filepath.Join(dir, "modules/wiki/services/worker")
		require.NoError(t, os.MkdirAll(path, 0700))
		require.NoError(t, os.WriteFile(filepath.Join(path, "service.codefly.yaml"), []byte(`kind: service
name: worker
version: 0.0.0
agent:
    kind: runtime::service
    name: go
    version: 0.0.1
    publisher: codefly.dev
service-dependencies:
    - name: gateway
      module: host
      endpoints:
          - name: rest
`), 0600))
	})
	worker, err := flow.originModule.LoadServiceFromName(t.Context(), "worker")
	require.NoError(t, err)
	flow.originService = worker
	require.NoError(t, flow.includeConsumedRoots(t.Context()))
	require.Empty(t, flow.consumedAPIs)
	order, err := flow.runClosure(t.Context())
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"host/gateway"}, uniques(order))
	flow.world.setRunProducers(uniques(order), flow.originService)
	routes, err := flow.world.hostRoutes(t.Context(), "host")
	require.NoError(t, err, "an unselected solution entry must not inject its consumes into routes")
	require.Empty(t, routes)
}

func TestConsumedClosureWithdrawConsumedRootActuallyStopsProducer(t *testing.T) {
	flow := selectedSolutionFlow(t, nil)
	module, err := flow.workspace.LoadModuleFromName(t.Context(), "host")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(t.Context(), "gateway")
	require.NoError(t, err)
	flow.originModule, flow.originService = module, service
	flow.coRoots = []string{"wiki/backend"}
	require.NoError(t, flow.includeConsumedRoots(t.Context()))
	flow.providedWorkspaceConfigurations = &configurations.WorkspaceConfigurations{}
	flow.hub = &Hub{}
	for _, unique := range flow.rootUniques() {
		svc, err := flow.workspace.LoadService(t.Context(), &resources.ServiceWithModule{Module: unique[:len(unique)-len(filepath.Base(unique))-1], Name: filepath.Base(unique)})
		require.NoError(t, err)
		flow.services = append(flow.services, svc)
		flow.hub.add(&phaseRecorder{recordingManager: recordingManager{unique: unique}})
	}
	flow.playbook = &Playbook{}
	flow.policy = &RuntimeStartPolicy{dependencies: flow.world.Dependencies, ExecutorManager: flow}
	stopped, err := flow.Withdraw(t.Context(), flow.workspace, []string{"wiki/backend"})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"wiki/backend", "docs/documents"}, stopped)
	require.Equal(t, []string{"host/gateway"}, flow.Roots())
}

func TestConsumedClosureRejectedWithdrawalRetainsRootsForRetry(t *testing.T) {
	flow := selectedSolutionFlow(t, func(dir string) {
		require.NoError(t, os.CopyFS(filepath.Join(dir, "modules/other"), os.DirFS(filepath.Join(dir, "modules/wiki"))))
		for _, path := range []string{"module.codefly.yaml", "services/backend/service.codefly.yaml"} {
			name := filepath.Join(dir, "modules/other", path)
			data, err := os.ReadFile(name)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(name, []byte(strings.ReplaceAll(string(data), "wiki", "other")), 0600))
		}
		name := filepath.Join(dir, "workspace.codefly.yaml")
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(name, append(data, []byte("    - name: other\n")...), 0600))
	})
	module, err := flow.workspace.LoadModuleFromName(t.Context(), "host")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(t.Context(), "gateway")
	require.NoError(t, err)
	flow.originModule, flow.originService = module, service
	flow.coRoots = []string{"wiki/backend", "other/backend"}
	require.NoError(t, flow.includeConsumedRoots(t.Context()))
	selected, selectErr := flow.runClosure(t.Context())
	require.NoError(t, selectErr)
	flow.world.setRunProducers(uniques(selected), flow.originService)
	flow.providedWorkspaceConfigurations = &configurations.WorkspaceConfigurations{}
	flow.hub = &Hub{}
	for _, unique := range flow.rootUniques() {
		id, err := resources.ParseServiceWithOptionalModule(unique)
		require.NoError(t, err)
		svc, err := flow.workspace.LoadService(t.Context(), id)
		require.NoError(t, err)
		flow.services = append(flow.services, svc)
		flow.hub.add(&phaseRecorder{recordingManager: recordingManager{unique: unique}})
	}
	flow.playbook = &Playbook{}
	flow.policy = &RuntimeStartPolicy{dependencies: flow.world.Dependencies, ExecutorManager: flow}
	name := filepath.Join(flow.workspace.Dir(), "modules/other/solution.codefly.yaml")
	valid, err := os.ReadFile(name)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(name, []byte(strings.ReplaceAll(string(valid), "endpoint: web", "endpoint: absent")), 0600))
	_, err = flow.Withdraw(t.Context(), flow.workspace, []string{"wiki/backend"})
	require.ErrorContains(t, err, "undeclared endpoint")
	require.Contains(t, flow.Roots(), "wiki/backend", "rejected withdrawal must remain discoverable by next SIGUSR1")
	require.True(t, flow.world.runProducers["wiki/backend"])
	require.NoError(t, os.WriteFile(name, valid, 0600))
	stopped, retryErr := flow.Withdraw(t.Context(), flow.workspace, []string{"wiki/backend"})
	require.NoError(t, retryErr)
	require.Equal(t, []string{"wiki/backend"}, stopped)
	require.NotContains(t, flow.Roots(), "wiki/backend")
	require.Contains(t, flow.Roots(), "docs/documents", "the remaining consumer retains its shared producer")
}

func TestConsumedClosureUnreadableUnselectedModuleCannotRejectRoutes(t *testing.T) {
	flow := selectedSolutionFlow(t, nil)
	require.NoError(t, flow.includeConsumedRoots(t.Context()))
	order, err := flow.runClosure(t.Context())
	require.NoError(t, err)
	flow.world.setRunProducers(uniques(order), flow.originService)
	flow.workspace.Modules = append(flow.workspace.Modules, &resources.ModuleReference{Name: "unreadable"})
	_, err = flow.world.hostRoutes(t.Context(), "host")
	require.NoError(t, err)
}
