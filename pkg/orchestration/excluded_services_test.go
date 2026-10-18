package orchestration

import (
	"context"
	"testing"

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
