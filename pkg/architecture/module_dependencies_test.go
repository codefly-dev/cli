package architecture_test

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestPublicModuleGraph(t *testing.T) {
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, "testdata/module-layout")
	require.NoError(t, err)
	require.NotNil(t, workspace)

	// modules:
	// management:
	// - organization [module endpoint]
	// web:
	// - frontend -> gateway [public http]
	// - gateway -> organization [public rest]
	// billing
	// - accounts [public rest]
	//

	require.Equal(t, 3, len(workspace.Modules))
	gs, err := architecture.LoadPublicModuleGraph(ctx, workspace)
	require.NoError(t, err)
	require.Equal(t, 2, len(gs))

	groups := map[string]*architecture.DAG{}
	for _, g := range gs {
		groups[g.Name] = g
	}
	billing := groups["billing"]
	require.NotNil(t, billing)

	// Should have
	// billing -> billing/accounts -> billing/accounts/rest
	require.Equal(t, 3, len(billing.Nodes()))
	require.Equal(t, 2, len(billing.Edges()))

	{
		expectedWebNodes := []*architecture.Node{
			{
				ID:   "billing",
				Type: resources.MODULE,
			},
			{
				ID:   "billing/accounts",
				Type: resources.SERVICE,
			},
			{
				ID:   "billing/accounts/rest",
				Type: resources.ENDPOINT,
			},
		}
		for _, expected := range expectedWebNodes {
			found := false
			for _, node := range billing.Nodes() {
				if node.ID == expected.ID && node.Type == expected.Type {
					found = true
				}
			}
			require.True(t, found)
		}

	}
	{
		expectedWebEdges := []*architecture.Edge{
			{
				From: "billing",
				To:   "billing/accounts",
			},
			{
				From: "billing/accounts",
				To:   "billing/accounts/rest",
			},
		}
		for _, expected := range expectedWebEdges {
			found := false
			for _, edge := range billing.Edges() {
				if edge.From == expected.From && edge.To == expected.To {
					found = true
				}
			}
			require.True(t, found)
		}
	}
	web := groups["web"]
	require.NotNil(t, web)

	// Should have
	// web -> web/frontend -> web/frontend/rest (3 nodes)
	// web -> web/gateway -> web/gateway/rest (+2)
	// web -> web/gateway -> web/gateway/grpc (+1)
	require.Equal(t, 6, len(web.Nodes()))

	{
		expectedWebEdges := []*architecture.Edge{
			{
				From: "web",
				To:   "web/frontend",
			},
			{
				From: "web/frontend",
				To:   "web/frontend/http",
			},
			{
				From: "web",
				To:   "web/gateway",
			},
			{
				From: "web/gateway",
				To:   "web/gateway/rest",
			},
			{
				From: "web/gateway",
				To:   "web/gateway/grpc",
			},
		}
		require.Equal(t, len(expectedWebEdges), len(web.Edges()))
		for _, expected := range expectedWebEdges {
			found := false
			for _, edge := range web.Edges() {
				if edge.From == expected.From && edge.To == expected.To {
					found = true
				}
			}
			require.True(t, found, expected)
		}
	}

}
