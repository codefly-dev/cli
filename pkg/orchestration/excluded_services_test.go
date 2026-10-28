package orchestration

import (
	"context"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func exclusionWorkspaceFiles(declarations string) map[string]string {
	service := func(name, extra string) string {
		return `kind: service
name: ` + name + `
version: 0.0.0
module: shop
agent:
    kind: runtime::service
    name: go-grpc
    version: 0.0.1
    publisher: codefly.ai
` + extra
	}
	return map[string]string{
		"workspace.codefly.yaml": `name: acme
layout: modules
modules:
    - name: shop
environments:
    - name: staging
      namespace: acme
` + declarations,
		"modules/shop/module.codefly.yaml": `kind: module
name: shop
services:
    - name: api
    - name: vault
`,
		"modules/shop/services/api/service.codefly.yaml": service("api", `service-dependencies:
    - name: vault
`),
		"modules/shop/services/vault/service.codefly.yaml": service("vault", ""),
	}
}

// An environment's exclude-services takes the service out of the flow's graph,
// and the consumer's withdrawn edge with it: nothing orders, starts, builds or
// deploys it, and no address for it is handed to the consumer.
func TestNewFlowLeavesAnExcludedServiceOutOfTheGraph(t *testing.T) {
	workspace := writeTempWorkspace(t, exclusionWorkspaceFiles(`      exclude-services:
          - service: shop/vault
            withdrawn-from: [shop/api]
`))
	ctx := context.Background()
	module, err := workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	api, err := module.LoadServiceFromName(ctx, "api")
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, "staging")
	require.NoError(t, err)

	flow, err := NewFlow(ctx, workspace, module, api, env, RunMode)
	require.NoError(t, err)
	_, err = flow.world.Dependencies.ServiceFromUnique("shop/vault")
	require.Error(t, err, "the excluded service is in the graph")
	order, err := flow.world.Dependencies.OrderTo(ctx, "shop/api")
	require.NoError(t, err)
	for _, service := range order {
		require.NotEqual(t, "shop/vault", service.Unique)
	}
	require.Equal(t, []string{"shop/vault"}, flow.excludedDependencyServices)

	// The excluded service itself has nothing to run there.
	vault, err := module.LoadServiceFromName(ctx, "vault")
	require.NoError(t, err)
	_, err = NewFlow(ctx, workspace, module, vault, env, RunMode)
	require.ErrorContains(t, err, `environment "staging" excludes shop/vault`)
}

// A consumer the environment does not name as a withdrawal refuses the flow by
// name rather than handing it a dependency that is not there.
func TestNewFlowRefusesAnExclusionSomethingStillConsumes(t *testing.T) {
	workspace := writeTempWorkspace(t, exclusionWorkspaceFiles(`      exclude-services:
          - service: shop/vault
`))
	ctx := context.Background()
	module, err := workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	api, err := module.LoadServiceFromName(ctx, "api")
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, "staging")
	require.NoError(t, err)
	_, err = NewFlow(ctx, workspace, module, api, env, RunMode)
	require.ErrorContains(t, err, "shop/api declares a dependency on it")
}

// A reference reaching a consumer only through a composition-root group (one
// the consumer does not declare) is judged against the whole workspace — less
// what the environment excludes: such a producer has no address anywhere in
// it. (Independent review of #1057, finding M1.)
func TestPlanRefusesARootGroupReferenceToAnExcludedService(t *testing.T) {
	ctx := context.Background()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n" +
			"environments:\n    - name: staging\n      namespace: boundary\n      exclude-services:\n          - service: platform/authority\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nservices:\n    - name: authority\n",
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: admin\n      api: rest\n      visibility: public\n" + statedExposure("public"),
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nservices:\n    - name: worker\n",
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
		"configurations/staging/work-context.env": "authority-endpoint=${endpoint:platform/authority/admin}\n",
	})
	env, err := SelectEnvironment(workspace, "staging")
	require.NoError(t, err)
	consumer, err := loadService(ctx, t, workspace, "payments", "worker")
	require.NoError(t, err)
	require.Empty(t, consumer.WorkspaceConfigurationDependencies)

	err = PlanConfigurationReferences(ctx, workspace, env, []*resources.Service{consumer}, false)
	require.ErrorContains(t, err, "from this environment (exclude-services")
	require.ErrorContains(t, err, "platform/authority")
}

// TestFlowIsNotRefusedOverAnotherEnvironmentsExclusion is the Blue render
// defect: a flow selecting one environment was refused over a DIFFERENT
// environment's exclude-services.
//
// The exclusion graph is built from the workspace's current composition, so an
// environment imported before a module overlay added a service carries no
// withdrawal for it. Holding every environment to that graph meant a correct
// `--env blue` render failed over `staging`, whose composition predates the
// service — and the operator could not fix it from the declaration side,
// because the product intent declares no `staging` to re-import.
//
// Here `stale` excludes shop/vault while shop/api still consumes it, so
// `stale` genuinely does not hold up. Selecting `staging`, whose own
// declaration is clean, must still succeed.
func TestFlowIsNotRefusedOverAnotherEnvironmentsExclusion(t *testing.T) {
	ctx := context.Background()
	files := exclusionWorkspaceFiles("")
	files["workspace.codefly.yaml"] = `name: acme
layout: modules
modules:
    - name: shop
environments:
    - name: staging
      namespace: acme
    - name: stale
      namespace: acme
      exclude-services:
          - service: shop/vault
`
	workspace := writeTempWorkspace(t, files)
	module, err := workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	api, err := module.LoadServiceFromName(ctx, "api")
	require.NoError(t, err)

	// The other environment really is inconsistent with the graph: selecting it
	// refuses, which is what makes the next assertion meaningful rather than
	// vacuous.
	stale, err := SelectEnvironment(workspace, "stale")
	require.NoError(t, err)
	_, err = NewFlow(ctx, workspace, module, api, stale, RunMode)
	require.ErrorContains(t, err, "declares a dependency on it",
		"the inconsistent environment must still refuse when it is the selected one")

	// Selecting the clean environment must not inherit that refusal.
	staging, err := SelectEnvironment(workspace, "staging")
	require.NoError(t, err)
	_, err = NewFlow(ctx, workspace, module, api, staging, RunMode)
	require.NoError(t, err, "a flow is held to its own environment, never refused over another's")
}
