package orchestration

import (
	"context"
	"testing"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// multiRootFlow builds a flow over the multi-root fixture: two solution
// backends (lastlogin-go, wiki) that both require the saas host services, wiki
// additionally requiring documents, one service (analytics/reports) the
// workspace declares but no root selects, and two build-only edges
// (wiki/backend → contracts/schemas, documents/documents → saas/auth-gateway)
// that must never constrain a run.
//
// It applies selectDependencyStage, as InitManagers does before it computes the
// run set: without that the graph still carries build-stage edges, and a test
// reading it would not be looking at the graph the run actually uses.
func multiRootFlow(t *testing.T, origin string, coRoots ...string) (*Flow, context.Context) {
	t.Helper()
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, "testdata/multi-root")
	require.NoError(t, err)
	parsed, err := resources.ParseServiceWithOptionalModule(origin)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, parsed.Module)
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, parsed.Name)
	require.NoError(t, err)
	dependencies, err := architecture.NewServiceDependencies(ctx, workspace)
	require.NoError(t, err)
	flow := &Flow{
		workspace:     workspace,
		originService: service,
		originModule:  module,
		world:         &World{Dependencies: dependencies, Mode: RunMode},
	}
	require.NoError(t, flow.selectDependencyStage())
	return flow.WithCoRoots(coRoots...), ctx
}

func uniques(services []architecture.Service) []string {
	out := make([]string, 0, len(services))
	for _, service := range services {
		out = append(out, service.Unique)
	}
	return out
}

// A composition that selects several solutions runs them against one graph: the
// host services they share are resolved once, and every co-root is managed by
// the same flow rather than needing a second `codefly run`.
func TestRunClosureUnionsEveryRootOverOneGraph(t *testing.T) {
	flow, ctx := multiRootFlow(t, "lastlogin-go/backend", "wiki/backend")

	required, err := flow.runClosure(ctx)
	require.NoError(t, err)
	got := uniques(required)

	require.ElementsMatch(t,
		[]string{"saas/frontend", "saas/auth-gateway", "documents/documents", "wiki/backend"},
		got,
		"the run set is the union of both roots' closures, plus the co-root itself")
	require.NotContains(t, got, "lastlogin-go/backend", "the origin gets its manager separately")
	require.NotContains(t, got, "analytics/reports", "a service no root selects is not in the run")

	// The shared host services are started once, not once per solution: two
	// graphs are exactly what collides on ports and container-recovery scope.
	require.Len(t, got, 4)

	// Dependencies precede the root that requires them, so managers are created
	// in the order the run set is meant to come up in.
	require.Less(t, indexOf(t, got, "saas/frontend"), indexOf(t, got, "wiki/backend"))
	require.Less(t, indexOf(t, got, "documents/documents"), indexOf(t, got, "wiki/backend"))
}

// With one root the union must reduce to exactly what the run path computed
// before co-roots existed, so an ordinary `codefly run service` is untouched.
func TestRunClosureWithOneRootMatchesOrderTo(t *testing.T) {
	flow, ctx := multiRootFlow(t, "wiki/backend")

	required, err := flow.runClosure(ctx)
	require.NoError(t, err)
	order, err := flow.world.Dependencies.OrderTo(ctx, "wiki/backend")
	require.NoError(t, err)

	require.Equal(t, uniques(order), uniques(required))
}

// A run seeded from several roots cannot narrow its graph to any one of them,
// so the graph is scoped to the run's own service set instead. Without that,
// propagating an endpoint change to a dependent could reach a service the flow
// has no manager for.
func TestScopeDependenciesToRunDropsServicesNoRootSelected(t *testing.T) {
	flow, ctx := multiRootFlow(t, "lastlogin-go/backend", "wiki/backend")

	required, err := flow.runClosure(ctx)
	require.NoError(t, err)
	require.NoError(t, flow.scopeDependenciesToRun(ctx, uniques(required), nil))

	dependents, err := flow.world.Dependencies.DirectDependents(ctx, "saas/frontend")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"lastlogin-go/backend", "wiki/backend"}, uniques(dependents),
		"both roots consume the shared host service, and nothing outside the run does")

	_, err = flow.world.Dependencies.ServiceFromUnique("analytics/reports")
	require.Error(t, err, "a service outside the run is not resolvable through the run's graph")
}

// Scoping the graph to the run set rebuilds it from the workspace, which reloads
// every edge kind — including the build-stage ones selectDependencyStage drops.
// A `build` or `schema` edge between two services that are BOTH in the run set
// (here documents/documents → saas/auth-gateway) is the case that survives the
// exclusion of out-of-run services, so it is the one that comes back as a
// runtime ordering constraint if the stage selection is not re-applied. Paired
// with a runtime edge the other way it makes the run graph cyclic and the run
// fails outright.
func TestScopeDependenciesToRunKeepsBuildOnlyEdgesOutOfTheRunGraph(t *testing.T) {
	flow, ctx := multiRootFlow(t, "lastlogin-go/backend", "wiki/backend")

	required, err := flow.runClosure(ctx)
	require.NoError(t, err)
	require.NoError(t, flow.scopeDependenciesToRun(ctx, uniques(required), nil))

	requires, err := flow.world.Dependencies.DirectRequires(ctx, "documents/documents")
	require.NoError(t, err)
	require.NotContains(t, uniques(requires), "saas/auth-gateway",
		"a build-only edge between two run-set services must not order the run")
}

// A build-only dependency on a service nothing else needs at runtime must not
// drag that service into the run at all.
func TestRunClosureExcludesBuildOnlyDependencies(t *testing.T) {
	flow, ctx := multiRootFlow(t, "lastlogin-go/backend", "wiki/backend")

	required, err := flow.runClosure(ctx)
	require.NoError(t, err)
	require.NotContains(t, uniques(required), "contracts/schemas",
		"wiki/backend declares contracts/schemas as a build dependency only")
}

func TestRootBeginActionsSeedEveryRoot(t *testing.T) {
	flow, _ := multiRootFlow(t, "lastlogin-go/backend", "wiki/backend")

	require.Equal(t, []Action{
		{Type: RuntimeBegin, Service: "lastlogin-go/backend"},
		{Type: RuntimeBegin, Service: "wiki/backend"},
	}, flow.rootBeginActions())
}

// --load-only / --init-only stop the playbook at a barrier. With several roots
// the first one to reach it must not end the run, or the other roots are never
// carried that far.
func TestStopAfterRootsWaitsForEveryRoot(t *testing.T) {
	ctx := context.Background()
	stop := stopAfterRoots([]string{"lastlogin-go/backend", "wiki/backend"}, RuntimeLoad)

	require.False(t, stop(ctx, Action{Type: RuntimeLoad, Service: "saas/frontend"}), "a dependency is not a root")
	require.False(t, stop(ctx, Action{Type: RuntimeInit, Service: "lastlogin-go/backend"}), "another phase is not the barrier")
	require.False(t, stop(ctx, Action{Type: RuntimeLoad, Service: "lastlogin-go/backend"}), "one root is not every root")
	require.True(t, stop(ctx, Action{Type: RuntimeLoad, Service: "wiki/backend"}))
}

func TestStopAfterRootsWithOneRootStopsAtItsBarrier(t *testing.T) {
	ctx := context.Background()
	stop := stopAfterRoots([]string{"wiki/backend"}, RuntimeInit)

	require.False(t, stop(ctx, Action{Type: RuntimeInit, Service: "saas/frontend"}))
	require.True(t, stop(ctx, Action{Type: RuntimeInit, Service: "wiki/backend"}))
}

// Readiness covers every root, not just the one the run is named after: a run
// that reports ready while a co-root has not started is the failure mode the
// hand-wired processes had.
func TestReadinessRequirementsCoverEveryRoot(t *testing.T) {
	flow, ctx := multiRootFlow(t, "lastlogin-go/backend", "wiki/backend")

	requirements, err := flow.readinessRequirements(ctx)
	require.NoError(t, err)

	var lifecycles []string
	for _, requirement := range requirements {
		if requirement.predicate == PredicateLifecycle {
			lifecycles = append(lifecycles, requirement.service)
		}
	}
	require.ElementsMatch(t, []string{
		"lastlogin-go/backend", "wiki/backend",
		"saas/frontend", "saas/auth-gateway", "documents/documents",
	}, lifecycles)
}

// Every root is a consumer of the graph, so a co-root's declaration of an
// endpoint its producer does not expose fails the run with the manifest error
// rather than waiting forever for a mapping that can never appear.
func TestDependencyEndpointDeclarationsValidateCoRoots(t *testing.T) {
	flow, ctx := multiRootFlow(t, "lastlogin-go/backend", "wiki/backend")

	required, err := flow.runClosure(ctx)
	require.NoError(t, err)
	require.NoError(t, flow.validateDependencyEndpointDeclarations(uniques(required)))
}

// The shutdown view names exactly what gets torn down. Solutions conventionally
// name their entry service the same thing — the composition in #717 has three
// `backend`s — so identifying a managed service by its bare name dropped every
// co-root that shared the origin's name, and rendered the survivors
// ambiguously.
func TestManagedServicesNamesEveryRootItTearsDown(t *testing.T) {
	flow, ctx := multiRootFlow(t, "lastlogin-go/backend", "wiki/backend")

	required, err := flow.runClosure(ctx)
	require.NoError(t, err)
	for _, service := range required {
		resolved, err := flow.world.Dependencies.ServiceFromUnique(service.Unique)
		require.NoError(t, err)
		flow.services = append(flow.services, resolved)
	}
	flow.services = append(flow.services, flow.originService)

	origin, dependencies := flow.ManagedServices()
	require.Equal(t, "lastlogin-go/backend", origin)
	require.Contains(t, dependencies, "wiki/backend",
		"a co-root sharing the origin's bare service name is still torn down, so it must be named")
	require.ElementsMatch(t,
		[]string{"saas/frontend", "saas/auth-gateway", "documents/documents", "wiki/backend"},
		dependencies)
}

func indexOf(t *testing.T, values []string, want string) int {
	t.Helper()
	for i, value := range values {
		if value == want {
			return i
		}
	}
	t.Fatalf("%q not found in %v", want, values)
	return -1
}
