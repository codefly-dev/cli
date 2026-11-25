package orchestration

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	agentv1 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v1"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// sharedServerWorkspace is a composition of two modules that each own a
// Postgres store, pinned as the case says, and an API that depends on both
// stores and on a Go worker of the other module. The API and the worker run
// one agent at two versions on purpose: they share nothing.
func sharedServerWorkspace(t *testing.T, evaluationPin, scenariosPin, overrides string) *resources.Workspace {
	t.Helper()
	service := func(module, name, agent, version, rest string) string {
		return "kind: service\nname: " + name + "\nversion: 0.0.0\nmodule: " + module + "\n" +
			"agent:\n    kind: codefly:service\n    name: " + agent + "\n    version: " + version + "\n    publisher: codefly.dev\n" + rest
	}
	return writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: stores\nlayout: modules\nmodules:\n    - name: evaluation\n    - name: scenarios\n" + overrides,
		"modules/evaluation/module.codefly.yaml": "kind: module\nname: evaluation\n" +
			"services:\n    - name: store\n    - name: worker\n",
		"modules/evaluation/services/store/service.codefly.yaml":  service("evaluation", "store", "postgres", evaluationPin, ""),
		"modules/evaluation/services/worker/service.codefly.yaml": service("evaluation", "worker", "go-grpc", "0.0.15", ""),
		"modules/scenarios/module.codefly.yaml": "kind: module\nname: scenarios\n" +
			"services:\n    - name: store\n    - name: api\n",
		"modules/scenarios/services/store/service.codefly.yaml": service("scenarios", "store", "postgres", scenariosPin, ""),
		"modules/scenarios/services/api/service.codefly.yaml": service("scenarios", "api", "go-grpc", "0.0.16",
			"service-dependencies:\n    - name: store\n      module: scenarios\n    - name: store\n      module: evaluation\n    - name: worker\n      module: evaluation\n"),
	})
}

// sharedServerFlow is the real Flow `codefly run service scenarios/api` builds
// in the given runtime context, and its run set as InitManagers orders it.
func sharedServerFlow(t *testing.T, workspace *resources.Workspace, runtimeContext string) (*Flow, []string) {
	t.Helper()
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	ctx := context.Background()
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	scenarios, err := workspace.LoadModuleFromName(ctx, "scenarios")
	require.NoError(t, err)
	api, err := scenarios.LoadServiceFromName(ctx, "api")
	require.NoError(t, err)
	flow, err := NewFlow(ctx, workspace, scenarios, api, env, RunMode)
	require.NoError(t, err)
	t.Cleanup(func() { _ = flow.Stop() })
	flow.WithRuntimeContext(runtimeContext)
	order, err := flow.managerDependencies(ctx)
	require.NoError(t, err)
	var required []string
	for _, service := range order {
		required = append(required, service.Unique)
	}
	require.ElementsMatch(t, []string{"evaluation/store", "evaluation/worker", "scenarios/store"}, required,
		"the fixture must put both stores in the run")
	return flow, required
}

// The defect: two modules pin the Postgres agent at two versions and one nix
// run attaches both stores to its one server. The run is refused by
// InitManagers itself, before a single agent is spawned, and the refusal names
// each service with its version, the server they share and the line to add.
func TestARunRefusesTwoVersionsOfTheAgentOfItsSharedServer(t *testing.T) {
	flow, _ := sharedServerFlow(t, sharedServerWorkspace(t, "0.0.150", "0.0.152", ""), resources.RuntimeContextNix)

	err := flow.InitManagers(context.Background())
	var refused *SharedRunServerError
	require.True(t, errors.As(err, &refused), "a mixed shared server must refuse the run, got %v", err)
	require.Empty(t, flow.hub.managers, "nothing may be spawned before the refusal")

	require.Len(t, refused.Conflicts, 1)
	require.Equal(t, []SharedRunServerService{
		{Unique: "evaluation/store", Version: "0.0.150"},
		{Unique: "scenarios/store", Version: "0.0.152"},
	}, refused.Conflicts[0].Services)
	require.Equal(t, "cannot start the run: "+
		"evaluation/store runs codefly.dev/postgres 0.0.150, scenarios/store runs codefly.dev/postgres 0.0.152, "+
		"and they share this run's one Postgres server: every version of the agent keeps its own record of that server, "+
		"so one version refuses the server another started; "+
		"run them on one version: add `agent-overrides: {codefly.dev/postgres: 0.0.152}` to workspace.codefly.yaml "+
		"(`codefly update workspace --agent-override codefly.dev/postgres=0.0.152` writes it), "+
		"or pin agent version 0.0.152 in each service's service.codefly.yaml", err.Error())
}

// One version on the shared server is a run like any other, and two versions
// of an agent that shares nothing (go-grpc 0.0.15 and 0.0.16 here) are never
// this check's business.
func TestARunAcceptsOneVersionOnItsSharedServer(t *testing.T) {
	flow, required := sharedServerFlow(t, sharedServerWorkspace(t, "0.0.152", "0.0.152", ""), resources.RuntimeContextNix)
	require.NoError(t, flow.checkSharedRunServers(required, nil))
}

// The committed remedy: the modules still pin two versions, and the
// workspace's agent-overrides puts both stores on one.
func TestAnAgentOverrideConvergesTheSharedServer(t *testing.T) {
	workspace := sharedServerWorkspace(t, "0.0.150", "0.0.152", "agent-overrides:\n    codefly.dev/postgres: 0.0.152\n")
	flow, required := sharedServerFlow(t, workspace, resources.RuntimeContextNix)
	require.NoError(t, flow.checkSharedRunServers(required, nil))
}

// Only the contexts the agent shares a server in are judged: in a container
// each store is a server of its own, and under "free" nothing is known until
// the agents have advertised their backends — the second call judges it then.
func TestTwoVersionsAreNotRefusedWhereNoServerIsShared(t *testing.T) {
	for _, runtimeContext := range []string{resources.RuntimeContextContainer, resources.RuntimeContextFree} {
		t.Run(runtimeContext, func(t *testing.T) {
			flow, required := sharedServerFlow(t, sharedServerWorkspace(t, "0.0.150", "0.0.152", ""), runtimeContext)
			require.NoError(t, flow.checkSharedRunServers(required, nil))
		})
	}
}

// A "free" run whose stores resolved to nix (Docker stopped, say) is the same
// shared server: once the loaded agents have their backends, the run set is
// judged again and refused before anything is initialized.
func TestAFreeRunIsRefusedOnceItsStoresResolveToTheSharedServer(t *testing.T) {
	flow, required := sharedServerFlow(t, sharedServerWorkspace(t, "0.0.150", "0.0.152", ""), resources.RuntimeContextFree)
	flow.dockerProbed = true
	flow.hub = &Hub{managers: []IManager{
		fakeManager("evaluation", "store", resources.RuntimeContextFree, agentv1.Backend_NIX, agentv1.Backend_DOCKER),
		fakeManager("scenarios", "store", resources.RuntimeContextFree, agentv1.Backend_NIX, agentv1.Backend_DOCKER),
	}}
	t.Cleanup(func() { flow.hub = &Hub{} })
	require.NoError(t, flow.checkSharedRunServers(required, nil), "nothing is shared while the backend is undecided")

	require.NoError(t, flow.resolveDockerFallback(context.Background()))
	var refused *SharedRunServerError
	require.True(t, errors.As(flow.checkSharedRunServers(required, nil), &refused))
	require.Equal(t, "0.0.152", refused.Conflicts[0].Newest())
}

// A store bound to a remote environment starts nothing here, and a store the
// run does not include shares nothing with it.
func TestOnlyTheLocalRunSetSharesAServer(t *testing.T) {
	flow, required := sharedServerFlow(t, sharedServerWorkspace(t, "0.0.150", "0.0.152", ""), resources.RuntimeContextNix)
	require.NoError(t, flow.checkSharedRunServers(required, map[string]*Remote{"evaluation/store": {}}))
	require.NoError(t, flow.checkSharedRunServers([]string{"scenarios/store", "evaluation/worker"}, nil))
}

// The same judgement with no run to ask, as the doctor makes it, and the
// version the remedy proposes: the newest, never the lexically greatest.
func TestSharedRunServerConflictsProposeTheNewestVersion(t *testing.T) {
	service := func(module, name, publisher, agent, version string) *resources.Service {
		out := &resources.Service{Name: name,
			Agent: &resources.Agent{Kind: resources.ServiceAgent, Publisher: publisher, Name: agent, Version: version}}
		out.WithModule(module)
		return out
	}
	conflicts := SharedRunServerConflicts([]*resources.Service{
		service("a", "store", "codefly.dev", "postgres", "0.0.99"),
		service("b", "store", "codefly.dev", "postgres", "0.0.152"),
		service("a", "cache", "codefly.dev", "redis", "0.0.100"),
		service("b", "cache", "codefly.dev", "redis", "0.0.101"),
		service("c", "store", "elsewhere.dev", "postgres", "9.9.9"),
	}, nil)
	require.Len(t, conflicts, 1, "redis keeps a server per service, and another publisher's postgres is another agent")
	require.Equal(t, "codefly.dev/postgres", conflicts[0].Agent)
	require.Equal(t, "0.0.152", conflicts[0].Newest())
}

// admittedStoreWorkspace is a composition whose two modules each own a
// Postgres store, pinned as the case says, and a Go service that depends on
// its own module's store alone: a run of evaluation/worker holds one store,
// and scenarios/api, admitted to it, brings the other.
func admittedStoreWorkspace(t *testing.T, evaluationPin, scenariosPin string) *resources.Workspace {
	t.Helper()
	service := func(module, name, agent, version, rest string) string {
		return "kind: service\nname: " + name + "\nversion: 0.0.0\nmodule: " + module + "\n" +
			"agent:\n    kind: codefly:service\n    name: " + agent + "\n    version: " + version + "\n    publisher: codefly.dev\n" + rest
	}
	module := func(name, root, pin string) map[string]string {
		return map[string]string{
			"modules/" + name + "/module.codefly.yaml":                 "kind: module\nname: " + name + "\nservices:\n    - name: store\n    - name: " + root + "\n",
			"modules/" + name + "/services/store/service.codefly.yaml": service(name, "store", "postgres", pin, ""),
			"modules/" + name + "/services/" + root + "/service.codefly.yaml": service(name, root, "go-grpc", "0.0.16",
				"service-dependencies:\n    - name: store\n      module: "+name+"\n"),
		}
	}
	files := map[string]string{
		"workspace.codefly.yaml": "name: stores\nlayout: modules\nmodules:\n    - name: evaluation\n    - name: scenarios\n",
	}
	for _, manifests := range []map[string]string{module("evaluation", "worker", evaluationPin), module("scenarios", "api", scenariosPin)} {
		for path, content := range manifests {
			files[path] = content
		}
	}
	return writeTempWorkspace(t, files)
}

// runningWorkerFlow is the real Flow `codefly run service evaluation/worker`
// builds in the given runtime context, as it stands once the run is up: its
// store and the worker are managed, the playbook is live, and a service
// admitted later is spawned through the returned record instead of an agent.
func runningWorkerFlow(t *testing.T, workspace *resources.Workspace, runtimeContext string, manager func(*resources.Service) IManager) (*Flow, *[]string) {
	t.Helper()
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	ctx := context.Background()
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	evaluation, err := workspace.LoadModuleFromName(ctx, "evaluation")
	require.NoError(t, err)
	worker, err := evaluation.LoadServiceFromName(ctx, "worker")
	require.NoError(t, err)
	flow, err := NewFlow(ctx, workspace, evaluation, worker, env, RunMode, WithRunModuleClosure("evaluation"))
	require.NoError(t, err)
	flow.WithRuntimeContext(runtimeContext)
	order, err := flow.managerDependencies(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"evaluation/store"}, uniques(order), "the run starts with one store")
	store, err := flow.ServiceFromUnique("evaluation/store")
	require.NoError(t, err)
	flow.hub = &Hub{managers: []IManager{manager(store), manager(worker)}}
	t.Cleanup(func() { flow.hub = &Hub{} })
	flow.playbook = &Playbook{}
	flow.policy = &RuntimeStartPolicy{dependencies: flow.world.Dependencies, ExecutorManager: flow}
	spawned := &[]string{}
	flow.spawn = func(_ context.Context, _ *resources.Module, service *resources.Service) (IManager, error) {
		*spawned = append(*spawned, resources.WithUnique(service).Unique())
		return manager(service), nil
	}
	return flow, spawned
}

func recordedManager(service *resources.Service) IManager {
	return &phaseRecorder{recordingManager: recordingManager{unique: resources.WithUnique(service).Unique()}}
}

// The same defect, met late: a nix run holds evaluation/store at one version
// of the Postgres agent, and a reloaded composition adds a root whose store is
// pinned at another. Extend judges the admitted services with the ones already
// running and refuses with the run's own message before one of them is
// spawned; the run it was asked to extend is left as it was.
func TestExtendRefusesAnAdmittedStoreAtAnotherVersionOfTheSharedServersAgent(t *testing.T) {
	workspace := admittedStoreWorkspace(t, "0.0.150", "0.0.152")
	flow, spawned := runningWorkerFlow(t, workspace, resources.RuntimeContextNix, recordedManager)
	running := flow.hub.all()
	ctx := context.Background()
	reloaded, err := resources.LoadWorkspaceFromDir(ctx, workspace.Dir())
	require.NoError(t, err)

	added, err := flow.Extend(ctx, reloaded, []string{"scenarios/api"})
	var refused *SharedRunServerError
	require.True(t, errors.As(err, &refused), "a store admitted at another version must refuse the extension, got %v", err)
	require.Empty(t, added)
	require.Empty(t, *spawned, "nothing may be spawned before the refusal")
	require.Equal(t, running, flow.hub.all(), "the running services keep their managers")
	for _, manager := range running {
		recorder, ok := manager.(*phaseRecorder)
		require.True(t, ok)
		require.Empty(t, recorder.phases, recorder.unique+" was running and is left alone")
	}
	require.Equal(t, "cannot start the run: "+
		"evaluation/store runs codefly.dev/postgres 0.0.150, scenarios/store runs codefly.dev/postgres 0.0.152, "+
		"and they share this run's one Postgres server: every version of the agent keeps its own record of that server, "+
		"so one version refuses the server another started; "+
		"run them on one version: add `agent-overrides: {codefly.dev/postgres: 0.0.152}` to workspace.codefly.yaml "+
		"(`codefly update workspace --agent-override codefly.dev/postgres=0.0.152` writes it), "+
		"or pin agent version 0.0.152 in each service's service.codefly.yaml", err.Error())
}

// A store admitted at the version the run's server already runs joins it: the
// admitted closure is spawned and started, dependencies first.
func TestExtendAdmitsAStoreAtTheVersionOfTheSharedServersAgent(t *testing.T) {
	workspace := admittedStoreWorkspace(t, "0.0.152", "0.0.152")
	flow, spawned := runningWorkerFlow(t, workspace, resources.RuntimeContextNix, recordedManager)
	ctx := context.Background()
	reloaded, err := resources.LoadWorkspaceFromDir(ctx, workspace.Dir())
	require.NoError(t, err)

	added, err := flow.Extend(ctx, reloaded, []string{"scenarios/api"})
	require.NoError(t, err)
	require.Equal(t, []string{"scenarios/api"}, added)
	require.Equal(t, []string{"scenarios/store", "scenarios/api"}, *spawned)
	for _, manager := range flow.hub.all() {
		recorder, ok := manager.(*phaseRecorder)
		require.True(t, ok)
		if recorder.unique == "scenarios/store" || recorder.unique == "scenarios/api" {
			require.Equal(t, []ActionType{RuntimeLoad, RuntimeInit, RuntimeStart}, recorder.phases, recorder.unique)
		}
	}
}

// A "free" run shares nothing until its agents have advertised their backends,
// so an admitted store is spawned; once it resolves to nix beside the store
// already running there, the extension is refused before the admitted services
// are loaded, initialized or started.
func TestExtendRefusesOnceAnAdmittedFreeStoreResolvesToTheSharedServer(t *testing.T) {
	workspace := admittedStoreWorkspace(t, "0.0.150", "0.0.152")
	freeStore := func(service *resources.Service) IManager {
		if service.Name != "store" {
			return recordedManager(service)
		}
		identity, err := service.Identity()
		require.NoError(t, err)
		manager := fakeManager(identity.Module, identity.Name, resources.RuntimeContextFree, agentv1.Backend_NIX, agentv1.Backend_DOCKER)
		manager.service = service
		return manager
	}
	flow, spawned := runningWorkerFlow(t, workspace, resources.RuntimeContextFree, freeStore)
	flow.dockerProbed = true
	require.NoError(t, flow.resolveDockerFallback(context.Background()), "the running store fell back to nix")
	ctx := context.Background()
	reloaded, err := resources.LoadWorkspaceFromDir(ctx, workspace.Dir())
	require.NoError(t, err)

	_, err = flow.Extend(ctx, reloaded, []string{"scenarios/api"})
	var refused *SharedRunServerError
	require.True(t, errors.As(err, &refused), "an admitted store that resolves to the shared server must refuse the extension, got %v", err)
	require.Equal(t, []string{"scenarios/store", "scenarios/api"}, *spawned, "the backend is known only once the agents are spawned")
	for _, manager := range flow.hub.all() {
		if recorder, ok := manager.(*phaseRecorder); ok {
			require.Empty(t, recorder.phases, recorder.unique+" is never loaded, initialized or started")
		}
	}
}
