package architecture_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/configurations"
	"github.com/stretchr/testify/assert"
)

func TestServiceGraph(t *testing.T) {
	ctx := context.Background()
	ws := &configurations.Workspace{}
	project, err := ws.LoadProjectFromDir(ctx, "testdata/codefly-platform")
	assert.NoError(t, err)
	assert.NotNil(t, project)

	// applications:
	// management:
	// - organization
	// web:
	// - frontend -> gateway
	// - gateway -> organization
	// billing
	// - accounts

	assert.Equal(t, 3, len(project.Applications))
	g, err := architecture.LoadServiceGraph(ctx, project)
	assert.NoError(t, err)
	assert.NotNil(t, g)

	assert.Equal(t, 4, len(g.Nodes()))

	assert.Equal(t, 2, len(g.Edges()))

	expectedWebEdge := &architecture.Edge{
		From: "web/gateway", // is a dependency for
		To:   "web/frontend",
	}
	expectedManagementEdge := &architecture.Edge{
		From: "management/organization", // is a dependency for
		To:   "web/gateway",
	}
	for _, expected := range []*architecture.Edge{expectedWebEdge, expectedManagementEdge} {
		found := false
		for _, edge := range g.Edges() {
			if edge.From == expected.From && edge.To == expected.To {
				found = true
			}
		}
		assert.True(t, found)
	}

	children := g.TopologicalSortFrom("web/frontend")
	assert.True(t, reflect.DeepEqual(children, []string{"web/gateway", "management/organization"}))

	children = g.TopologicalSortFrom("web/gateway")
	assert.True(t, reflect.DeepEqual(children, []string{"management/organization"}))

	children = g.TopologicalSortFrom("management/organization")
	assert.Equal(t, 0, len(children))

}
