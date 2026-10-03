package orchestration

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// overrideOrderingWorkspace is a composition whose root group names one producer
// on disk and a different one through an invocation-scoped override. Both
// producers exist, both endpoints are public: nothing about either reference is
// illegal, which is the whole point — the fault is in which one the run was
// ordered for.
func overrideOrderingWorkspace(t *testing.T) *resources.Workspace {
	t.Helper()
	return writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: ordering\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: ordering\n" +
			"domain: github.com/codefly-ai/ordering/platform\nservices:\n    - name: authority\n    - name: gateway\n",
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: rest\n      api: rest\n      visibility: public\n",
		"modules/platform/services/gateway/service.codefly.yaml": "kind: service\nname: gateway\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: rest\n      api: rest\n      visibility: public\n",
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: ordering\n" +
			"domain: github.com/codefly-ai/ordering/payments\nservices:\n    - name: worker\n",
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"workspace-configuration-dependencies:\n    - work-context\n",
		"configurations/local/work-context.env": "producer-endpoint=${endpoint:platform/authority/rest}\n",
	})
}

// runClosureOf is the run set a flow of this origin computes: the origin and
// everything the dependency graph orders before it. It is read off the flow's
// own graph rather than written down, because the graph is the thing under test.
func runClosureOf(t *testing.T, flow *Flow, origin string) []string {
	t.Helper()
	order, err := flow.world.Dependencies.OrderTo(context.Background(), origin)
	require.NoError(t, err)
	uniques := []string{origin}
	for _, dependency := range order {
		if dependency.Unique != origin {
			uniques = append(uniques, dependency.Unique)
		}
	}
	return uniques
}

// An invocation-scoped override that redirects a reference to a different
// producer orders the run for the producer the value actually names.
//
// The graph and the gate used to read different things. `NewFlow` built the
// dependency graph from a plain read of `configurations/<profile>/`, which
// cannot see an override, while the plan gate validated the loaded — overridden
// — values. So a *valid* override pointing at a real, visible endpoint of a
// different producer produced this: the closure held the producer named on disk,
// the gate accepted the new reference because there is nothing wrong with it,
// and the resolution then dropped the value, because core drops a reference to a
// producer the run does not contain rather than failing the consumer. Delivered
// false, error nil — cli#882's own shape, reached through the one mechanism
// added to check for it. (Layer-4 round four, E1.)
//
// Both halves are asserted: the closure the graph computes, and what the public
// resolver delivers over that closure. The second is the one an operator meets;
// the first is where the fault is, and asserting only delivery would leave a
// future fix that papers over the ordering looking identical.
func TestAnInvocationOverrideOrdersTheRunForTheProducerItNames(t *testing.T) {
	ctx := context.Background()
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	workspace := overrideOrderingWorkspace(t)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	encoded, err := resources.EncodeWorkspaceConfigurationOverrides([]resources.WorkspaceConfigurationOverride{
		{Name: "work-context", Key: "producer-endpoint", Value: "${endpoint:platform/gateway/rest}"},
	})
	require.NoError(t, err)
	t.Setenv(resources.WorkspaceConfigurationOverridesEnvironment, encoded)

	payments, err := workspace.LoadModuleFromName(ctx, "payments")
	require.NoError(t, err)
	worker, err := payments.LoadServiceFromName(ctx, "worker")
	require.NoError(t, err)

	flow, err := NewFlow(ctx, workspace, payments, worker, env, RunMode)
	require.NoError(t, err)
	t.Cleanup(func() { _ = flow.Stop() })

	closure := runClosureOf(t, flow, "payments/worker")
	require.Contains(t, closure, "platform/gateway",
		"the run must be ordered for the producer the overridden value names")
	require.NotContains(t, closure, "platform/authority",
		"and not for the one only the directory still names")

	// And the value arrives. The run set is the closure the graph just computed
	// — not a hand-written one — so a wrongly ordered graph shows up here as the
	// silent drop it causes rather than as a disagreement about sets.
	flow.WithRuntimeContext(resources.RuntimeContextNative)
	require.NoError(t, flow.ConfigurationManager.Load(ctx, env.Runtime()))
	flow.world.setRunProducers(closure, nil)
	recordParityEndpoints(t, flow.world, "platform", "gateway")

	confs, err := flow.WorkspaceConfigurationsFor(ctx, worker)
	require.NoError(t, err)
	address, delivered := groupValue(confs, "work-context", "producer-endpoint")
	require.True(t, delivered,
		"the overridden reference must resolve: its producer is in the run the graph ordered for it")
	require.NotEmpty(t, address)
}
