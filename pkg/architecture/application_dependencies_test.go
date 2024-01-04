package architecture_test

import (
	"context"
	"testing"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/configurations"
	"github.com/stretchr/testify/assert"
)

func TestPublicApplicationGraph(t *testing.T) {
	ctx := context.Background()
	ws := &configurations.Workspace{}
	project, err := ws.LoadProjectFromDir(ctx, "testdata/codefly-platform")
	assert.NoError(t, err)
	assert.NotNil(t, project)

	// applications:
	// management:
	// - organization [application endpoint]
	// web:
	// - frontend -> gateway [public http]
	// - gateway -> organization [public rest]
	// billing
	// - accounts [public rest]
	//

	assert.Equal(t, 3, len(project.Applications))
	gs, err := architecture.LoadPublicApplicationGraph(ctx, project)
	assert.NoError(t, err)
	assert.Equal(t, 2, len(gs))

	groups := map[string]*architecture.Graph{}
	for _, g := range gs {
		groups[g.Name] = g
	}
	billing := groups["billing"]
	assert.NotNil(t, billing)
	// Should have
	// billing -> billing/accounts -> billing/accounts/rest
	assert.Equal(t, 3, len(billing.Nodes()))
	assert.Equal(t, 2, len(billing.Edges()))

	{
		expectedWebNodes := []*architecture.Node{
			{
				ID:   "billing",
				Type: configurations.APPLICATION,
			},
			{
				ID:   "billing/accounts",
				Type: configurations.SERVICE,
			},
			{
				ID:   "billing/accounts/rest",
				Type: configurations.ENDPOINT,
			},
		}
		for _, expected := range expectedWebNodes {
			found := false
			for _, node := range billing.Nodes() {
				if node.ID == expected.ID && node.Type == expected.Type {
					found = true
				}
			}
			assert.True(t, found)
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
			assert.True(t, found)
		}
	}
	web := groups["web"]
	assert.NotNil(t, web)
	// Should have
	// web -> web/frontend -> web/frontend/rest (3 nodes)
	// web -> web/gateway -> web/gateway/rest (+2)
	// web -> web/gateway -> web/gateway/grpc (+1)
	assert.Equal(t, 6, len(web.Nodes()))

	t.Log(web.Nodes())
	t.Log(web.Edges())

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
		assert.Equal(t, len(expectedWebEdges), len(web.Edges()))
		for _, expected := range expectedWebEdges {
			found := false
			for _, edge := range web.Edges() {
				if edge.From == expected.From && edge.To == expected.To {
					found = true
				}
			}
			assert.True(t, found, expected)
		}
	}

}
