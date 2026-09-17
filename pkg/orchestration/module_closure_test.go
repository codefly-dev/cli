package orchestration

import (
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func multiRootWorkspace(t *testing.T) *resources.Workspace {
	t.Helper()
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), "testdata/multi-root")
	require.NoError(t, err)
	return workspace
}

func moduleNames(workspace *resources.Workspace) []string {
	names := make([]string, 0, len(workspace.Modules))
	for _, ref := range workspace.Modules {
		names = append(names, ref.Name)
	}
	return names
}

// The whole point: a run carries the modules it reaches, not the modules the
// composition happens to pin. analytics is pinned and no root selects it;
// contracts is reached only by a build-stage edge, which a run never traverses.
func TestRunModuleClosureCarriesOnlyWhatTheRootReaches(t *testing.T) {
	workspace := multiRootWorkspace(t)

	scoped, err := runModuleClosure(t.Context(), workspace, []string{"wiki"})
	require.NoError(t, err)

	require.ElementsMatch(t, []string{"wiki", "saas", "documents"}, moduleNames(scoped))
	require.ElementsMatch(t,
		[]string{"analytics", "contracts", "documents", "lastlogin-go", "saas", "wiki"},
		moduleNames(workspace),
		"the pin set the composition committed is left alone")
}

// A product composition selects several solutions. Each root seeds the same
// closure, so the shared host module is carried once.
func TestRunModuleClosureUnionsEveryRoot(t *testing.T) {
	workspace := multiRootWorkspace(t)

	scoped, err := runModuleClosure(t.Context(), workspace, []string{"lastlogin-go", "wiki"})
	require.NoError(t, err)

	require.ElementsMatch(t, []string{"lastlogin-go", "wiki", "saas", "documents"}, moduleNames(scoped))
}

// Without seeds there is nothing to derive the graph from, so the run keeps the
// whole pin set — the behaviour every non-run entry point still has.
func TestRunModuleClosureWithoutSeedsKeepsTheWholePinSet(t *testing.T) {
	workspace := multiRootWorkspace(t)

	scoped, err := runModuleClosure(t.Context(), workspace, nil)
	require.NoError(t, err)

	require.Same(t, workspace, scoped)
}

// `codefly run` used to resolve cross-module edges the workspace-wide validator
// refuses, which is how a composition whose source check was red still came up.
// The run now applies the same rule to the graph it is about to start.
func TestRunModuleClosureRefusesWhatValidationRefuses(t *testing.T) {
	workspace := multiRootWorkspace(t)

	require.ErrorContains(t, workspace.ValidateServiceDependencies(t.Context()),
		`endpoint auth-gateway/admin is private to module "saas"`,
		"the fixture must carry the violation for this test to mean anything")

	_, err := runModuleClosure(t.Context(), workspace, []string{"analytics"})
	require.ErrorContains(t, err, `endpoint auth-gateway/admin is private to module "saas"`)
}

// The verdict is scoped to the run, not to the composition: an edge no root
// reaches belongs to the runs that do reach it and to the workspace-wide pass.
// Refusing it here would make a solution unrunnable because of a module it
// never touches.
func TestRunModuleClosureIgnoresAViolationOutsideTheRun(t *testing.T) {
	workspace := multiRootWorkspace(t)

	_, err := runModuleClosure(t.Context(), workspace, []string{"wiki"})
	require.NoError(t, err)
}

// A declaration reaching a module the composition does not pin is the drift the
// hand-maintained list hid: the run used to get as far as loading managers
// before failing on a module name, or come up with no endpoints at all. It is
// now refused up front, naming the service that asked.
func TestRunModuleClosureNamesTheServiceThatReachesAnUnpinnedModule(t *testing.T) {
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": `name: unpinned
layout: modules
modules:
    - name: app
`,
		"modules/app/module.codefly.yaml": `kind: module
name: app
project: unpinned
services:
    - name: backend
`,
		"modules/app/services/backend/service.codefly.yaml": `kind: service
name: backend
version: 0.0.0
module: app
agent:
    kind: runtime::service
    name: go-grpc
    version: 0.0.16
    publisher: codefly.ai
service-dependencies:
    - name: documents
      module: documents
`,
	})

	_, err := runModuleClosure(t.Context(), workspace, []string{"app"})
	require.ErrorContains(t, err, "app/backend")
	require.ErrorContains(t, err, `depends on module "documents"`)
}

// The graph the flow hands to every policy, manager and readiness probe is the
// closure's, and every later rebuild reads the same narrowed workspace — so a
// rebuild cannot widen the run back to modules no root reaches.
func TestNewFlowBuildsTheGraphFromTheRunClosure(t *testing.T) {
	ctx := t.Context()
	workspace := multiRootWorkspace(t)
	module, err := workspace.LoadModuleFromName(ctx, "wiki")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "backend")
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)

	flow, err := NewFlow(ctx, workspace, module, service, env, RunMode, WithRunModuleClosure("wiki"))
	require.NoError(t, err)

	_, err = flow.world.Dependencies.ServiceFromUnique("saas/frontend")
	require.NoError(t, err, "a module the root reaches is in the run's graph")
	_, err = flow.world.Dependencies.ServiceFromUnique("analytics/reports")
	require.Error(t, err, "a pinned module no root reaches is not in the run's graph")

	require.ElementsMatch(t, []string{"wiki", "saas", "documents"}, moduleNames(flow.graphWorkspace))
	require.Same(t, workspace, flow.workspace,
		"configurations and run profiles stay resolved against the whole composition")
}

// Every other entry point (build, test, deploy, sync) names no seeds and keeps
// the graph it had.
func TestNewFlowWithoutSeedsKeepsEveryPinnedModuleInTheGraph(t *testing.T) {
	ctx := t.Context()
	workspace := multiRootWorkspace(t)
	module, err := workspace.LoadModuleFromName(ctx, "wiki")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "backend")
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)

	flow, err := NewFlow(ctx, workspace, module, service, env, RunMode)
	require.NoError(t, err)

	_, err = flow.world.Dependencies.ServiceFromUnique("analytics/reports")
	require.NoError(t, err)
}

// The shape this exists for: a composition pins the modules it vendors — source
// and version for identity, the submodule checkout for location — and names only
// the solution it runs. The two modules that solution spans join the graph
// because it declares them, not because anyone listed them as participants.
func TestRunModuleClosureRunsASolutionSpanningTwoVendoredModules(t *testing.T) {
	ctx := t.Context()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": `name: platform
layout: modules
modules:
    - name: wiki
      source: acme/solutions
      version: "0.0.1"
      path: vendor/solutions/wiki
    - name: saas
      source: acme/lodestar
      version: "0.0.62"
      path: vendor/lodestar/saas
    - name: documents
      source: acme/lodestar
      version: "0.0.62"
      path: vendor/lodestar/documents
`,
		"vendor/solutions/wiki/module.codefly.yaml": `kind: module
name: wiki
project: platform
services:
    - name: backend
`,
		"vendor/solutions/wiki/services/backend/service.codefly.yaml": `kind: service
name: backend
version: 0.0.0
module: wiki
agent:
    kind: runtime::service
    name: go-grpc
    version: 0.0.16
    publisher: codefly.ai
service-dependencies:
    - name: auth-gateway
      module: saas
      endpoints:
        - name: rest
    - name: api
      module: documents
      endpoints:
        - name: grpc
endpoints:
    - name: http
      api: http
      visibility: public
`,
		"vendor/lodestar/saas/module.codefly.yaml": `kind: module
name: saas
project: platform
services:
    - name: auth-gateway
`,
		"vendor/lodestar/saas/services/auth-gateway/service.codefly.yaml": `kind: service
name: auth-gateway
version: 0.0.0
module: saas
agent:
    kind: runtime::service
    name: go-grpc
    version: 0.0.16
    publisher: codefly.ai
endpoints:
    - name: rest
      api: rest
      visibility: public
`,
		"vendor/lodestar/documents/module.codefly.yaml": `kind: module
name: documents
project: platform
services:
    - name: api
`,
		"vendor/lodestar/documents/services/api/service.codefly.yaml": `kind: service
name: api
version: 0.0.0
module: documents
agent:
    kind: runtime::service
    name: go-grpc
    version: 0.0.16
    publisher: codefly.ai
endpoints:
    - name: grpc
      api: grpc
      visibility: public
`,
	})

	// A pin carrying a committed path resolves to that checkout, so vendoring
	// needs no codefly.local.yaml to point the identity at what is already there.
	resolution, err := workspace.ResolveModule(ctx, workspace.Modules[1])
	require.NoError(t, err)
	require.Equal(t, resources.ResolutionLocalPath, resolution.Kind)

	module, err := workspace.LoadModuleFromName(ctx, "wiki")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "backend")
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)

	flow, err := NewFlow(ctx, workspace, module, service, env, RunMode, WithRunModuleClosure("wiki"))
	require.NoError(t, err)

	order, err := flow.world.Dependencies.OrderTo(ctx, "wiki/backend")
	require.NoError(t, err)
	got := make([]string, 0, len(order))
	for _, entry := range order {
		got = append(got, entry.Unique)
	}
	require.ElementsMatch(t, []string{"saas/auth-gateway", "documents/api"}, got,
		"both modules the solution spans are started for it, from a run that named only the solution")
}
