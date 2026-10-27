package orchestration

import (
	"context"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestRuntimePrerequisiteSurvivesClosureRebuildAndRootChanges(t *testing.T) {
	ctx := t.Context()
	w := multiRootWorkspace(t)
	module, err := w.LoadModuleFromName(ctx, "contracts")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "schemas")
	require.NoError(t, err)
	env, err := SelectEnvironment(w, LocalEnvironmentName)
	require.NoError(t, err)
	var closures [][]string
	provider := func(_ context.Context, scoped *resources.Workspace) (map[string][]string, error) {
		closures = append(closures, moduleNames(scoped))
		return map[string][]string{"contracts/schemas": {"saas/auth-gateway"}}, nil
	}
	flow, err := NewFlow(ctx, w, module, service, env, RunMode,
		WithRunModuleClosure("contracts"), WithRuntimePrerequisites([]string{"saas"}, provider))
	require.NoError(t, err)
	assertRequired := func() {
		t.Helper()
		require.ElementsMatch(t, []string{"contracts", "saas"}, closures[len(closures)-1])
		require.NoError(t, flow.selectDependencyStage())
		order, err := flow.world.Dependencies.OrderTo(ctx, "contracts/schemas")
		require.NoError(t, err)
		require.Contains(t, uniques(order), "saas/auth-gateway")
		origin, err := flow.world.Dependencies.ServiceFromUnique("contracts/schemas")
		require.NoError(t, err)
		require.Empty(t, origin.ServiceDependencies, "runtime ordering must not grant endpoint access")
	}
	assertRequired()
	_, options, err := flow.dependencyGraphOptions(ctx)
	require.NoError(t, err)
	require.NoError(t, flow.rebuildDependencyGraph(ctx, options))
	assertRequired()
	// Extension rebuilds from root seeds. The host survives without being an
	// authored service dependency or a user-selected root.
	_, err = flow.admitRoots(ctx, w, []string{"wiki/backend"})
	require.NoError(t, err)
	flow.hub = &Hub{}
	_, _, err = flow.rebuildExtendedGraph(ctx)
	require.NoError(t, err)
	require.Contains(t, moduleNames(flow.graphWorkspace), "saas")
	order, err := flow.world.Dependencies.OrderTo(ctx, "contracts/schemas")
	require.NoError(t, err)
	require.Contains(t, uniques(order), "saas/auth-gateway")
}

func TestRuntimePrerequisiteRefusesMissingOrExcludedHost(t *testing.T) {
	for _, host := range []string{"missing/service", "contracts/schemas"} {
		t.Run(host, func(t *testing.T) {
			w := multiRootWorkspace(t)
			module, err := w.LoadModuleFromName(t.Context(), "contracts")
			require.NoError(t, err)
			service, err := module.LoadServiceFromName(t.Context(), "schemas")
			require.NoError(t, err)
			env, err := SelectEnvironment(w, LocalEnvironmentName)
			require.NoError(t, err)
			_, err = NewFlow(t.Context(), w, module, service, env, RunMode, WithRunModuleClosure("contracts"),
				WithRuntimePrerequisites(nil, func(context.Context, *resources.Workspace) (map[string][]string, error) {
					return map[string][]string{"contracts/schemas": {host}}, nil
				}))
			require.ErrorContains(t, err, "runtime prerequisite")
		})
	}
	flow, ctx := multiRootFlow(t, "wiki/backend")
	flow.runtimePrerequisites = func(context.Context, *resources.Workspace) (map[string][]string, error) {
		return map[string][]string{"wiki/backend": {"saas/auth-gateway"}}, nil
	}
	flow.excludedDependencyServices = []string{"saas/auth-gateway"}
	_, options, err := flow.dependencyGraphOptions(ctx)
	require.NoError(t, err)
	require.ErrorContains(t, flow.rebuildDependencyGraph(ctx, options), "excluded")
}
