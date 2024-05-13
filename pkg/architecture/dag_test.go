package architecture_test

import (
	"testing"

	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/stretchr/testify/require"
)

func createNodes(s ...string) []architecture.Node {
	var out []architecture.Node
	for _, n := range s {
		out = append(out, architecture.Node{ID: n})
	}
	return out
}

func subgraphFrom(t *testing.T, g *architecture.DAG, node string) *architecture.DAG {
	sub, err := g.SubGraphFrom(node)
	require.NoError(t, err)
	return sub
}

func subgraphTo(t *testing.T, g *architecture.DAG, node string) *architecture.DAG {
	sub, err := g.SubGraphTo(node)
	require.NoError(t, err)
	return sub
}

func TestGraph(t *testing.T) {
	g := architecture.NewDAG("test")
	// Chain
	g.AddEdge("a", "b") // a -> b
	g.AddEdge("b", "c") // b -> c

	reachable := g.ReachableFrom("a", "c")
	require.True(t, reachable)

	// Branch
	g.AddEdge("d", "e") // d -> e
	g.AddEdge("d", "f") // d -> f

	// Inverse Branch
	g.AddEdge("x", "h") // x -> h
	g.AddEdge("y", "h") // y -> h
	g.AddEdge("x", "y") // x -> y

	// Lonely
	g.AddNode("z") // z

	require.Equal(t, 10, len(g.Nodes()))
	require.Equal(t, 7, len(g.Edges()))

	// Children

	children := g.Children("a")
	require.ElementsMatch(t, createNodes("b"), children, "Expected b")

	children = g.Children("d")
	require.ElementsMatch(t, createNodes("e", "f"), children, "Expected e, f")

	children = g.Children("x")
	require.ElementsMatch(t, createNodes("h", "y"), children, "Expected h, y")

	children = g.Children("z")
	require.Equal(t, 0, len(children))

	// Parents
	parents := g.Parents("a")
	require.Equal(t, 0, len(parents))

	parents = g.Parents("b")
	require.ElementsMatch(t, createNodes("a"), parents, "Expected a")

	parents = g.Parents("h")
	require.ElementsMatch(t, createNodes("x", "y"), parents, "Expected x, y")

	parents = g.Parents("z")
	require.Equal(t, 0, len(parents))

	// Topological sort
	order, err := g.TopologicalSortFrom("a")
	require.NoError(t, err)
	require.Equal(t, createNodes("b", "c"), order, "Expected b, c")

	order, err = g.TopologicalSortFrom("d")
	require.NoError(t, err)
	require.Equal(t, createNodes("e", "f"), order, "Expected e, f")

	order, err = g.TopologicalSortTo("h")
	require.NoError(t, err)
	require.Equal(t, createNodes("x", "y"), order, "Expected x, y")

	// Sub-graphs STARTING FROM NODE

	// a -> b -> c
	sub := subgraphFrom(t, g, "a")
	require.ElementsMatch(t, createNodes("a", "b", "c"), sub.Nodes(), "Expected a, b, c")

	// b -> c
	sub = subgraphFrom(t, g, "b")
	require.ElementsMatch(t, createNodes("b", "c"), sub.Nodes(), "Expected b, c")

	// c
	sub = subgraphFrom(t, g, "c")
	require.ElementsMatch(t, createNodes("c"), sub.Nodes(), "Expected c")

	// d
	sub = subgraphFrom(t, g, "d")
	require.ElementsMatch(t, createNodes("d", "e", "f"), sub.Nodes(), "Expected d, e, f")

	// z
	sub = subgraphFrom(t, g, "z")
	require.ElementsMatch(t, createNodes("z"), sub.Nodes(), "Expected z")

	// Sub-graphs ENDING AT NODE
	sub = subgraphTo(t, g, "a")
	require.ElementsMatch(t, createNodes("a"), sub.Nodes(), "Expected a")

	sub = subgraphTo(t, g, "b")
	require.ElementsMatch(t, createNodes("a", "b"), sub.Nodes(), "Expected a, b")

	sub = subgraphTo(t, g, "c")
	require.ElementsMatch(t, createNodes("a", "b", "c"), sub.Nodes(), "Expected a, b, c")

	sub = subgraphTo(t, g, "d")
	require.ElementsMatch(t, createNodes("d"), sub.Nodes(), "Expected d")

	sub = subgraphTo(t, g, "e")
	require.ElementsMatch(t, createNodes("d", "e"), sub.Nodes(), "Expected d, e")

	sub = subgraphTo(t, g, "f")
	require.ElementsMatch(t, createNodes("d", "f"), sub.Nodes(), "Expected d, f")

	sub = subgraphTo(t, g, "x")
	require.ElementsMatch(t, createNodes("x"), sub.Nodes(), "Expected x, h")

	sub = subgraphTo(t, g, "y")
	require.ElementsMatch(t, createNodes("y", "x"), sub.Nodes(), "Expected y, x")

	sub = subgraphTo(t, g, "h")
	require.ElementsMatch(t, createNodes("x", "y", "h"), sub.Nodes(), "Expected x, y, h")

	// z
	sub = subgraphTo(t, g, "z")
	require.ElementsMatch(t, createNodes("z"), sub.Nodes(), "Expected z")

}

type edgeInput struct {
	from string
	to   string
}

func permuteIndices(n int) [][]int {
	data := make([]int, n)
	for i := range data {
		data[i] = i
	}
	var result [][]int
	generatePermutations(data, 0, n, &result)
	return result
}

func generatePermutations(data []int, i int, length int, result *[][]int) {
	if i == length {
		permutation := make([]int, length)
		copy(permutation, data)
		*result = append(*result, permutation)
	} else {
		for j := i; j < length; j++ {
			swap(data, i, j)
			generatePermutations(data, i+1, length, result)
			swap(data, i, j) // backtrack
		}
	}
}

func swap(data []int, x int, y int) {
	data[x], data[y] = data[y], data[x]
}

func TestSortedChildren(t *testing.T) {
	operations := []edgeInput{
		{"a", "b"},
		{"a", "c"},
		{"a", "e"},
		{"b", "c"},
		{"c", "d"},
		{"d", "e"},
	}

	// Only topological compatible children is b, c, d, e

	for _, indices := range permuteIndices(len(operations)) {
		g := architecture.NewDAG("test")
		for _, i := range indices {
			g.AddEdge(operations[i].from, operations[i].to)
		}
		order, err := g.TopologicalSortFrom("a")
		require.NoError(t, err)
		require.Equal(t, createNodes("b", "c", "d", "e"), order)
		children, err := g.SortedChildren("a")
		require.NoError(t, err)
		require.Equal(t, createNodes("b", "c", "e"), children)
	}

}

func TestSortedParents(t *testing.T) {
	operations := []edgeInput{
		{"u", "z"},
		{"v", "z"},
		{"x", "z"},
		{"u", "w"},
		{"w", "v"},
		{"x", "u"},
	}

	// Only topological compatible children is x,u,w,v

	for _, indices := range permuteIndices(len(operations)) {
		g := architecture.NewDAG("test")
		for _, i := range indices {
			g.AddEdge(operations[i].from, operations[i].to)
		}
		order, err := g.TopologicalSortTo("z")
		require.NoError(t, err)
		require.Equal(t, createNodes("x", "u", "w", "v"), order)
		children, err := g.SortedParents("z")
		require.NoError(t, err)
		require.Equal(t, createNodes("x", "u", "v"), children)
	}
}

func TestSubGraphFrom(t *testing.T) {
	g := architecture.NewDAG("test")
	g.AddEdge("z", "u")
	g.AddEdge("z", "v")
	g.AddEdge("z", "x")
	g.AddEdge("v", "w")
	g.AddEdge("w", "u")
	g.AddEdge("u", "x")

	require.Equal(t, 5, len(g.Nodes()))
	require.Equal(t, 6, len(g.Edges()))

	sub := subgraphFrom(t, g, "z")
	require.Equal(t, 5, len(g.Nodes()))
	require.Equal(t, 6, len(g.Edges()))

	// All reachable from z
	require.ElementsMatch(t, createNodes("z", "u", "v", "w", "x"), sub.Nodes(), "Expected z, u, v, w, x")

	for _, node := range g.Nodes() {
		require.True(t, sub.HasNode(node.ID))
	}

	for _, edge := range g.Edges() {
		require.True(t, sub.HasEdge(edge.From, edge.To), "Expected edge %s -> %s", edge.From, edge.To)
	}

	order, err := g.TopologicalSort()
	require.NoError(t, err)
	require.Equal(t, createNodes("z", "v", "w", "u", "x"), order)

	order, err = sub.TopologicalSort()
	require.NoError(t, err)
	require.Equal(t, createNodes("z", "v", "w", "u", "x"), order)

	order, err = g.TopologicalSortFrom("z")
	require.NoError(t, err)
	require.Equal(t, createNodes("v", "w", "u", "x"), order)
}

func TestSubGraphTo(t *testing.T) {
	// Mirror
	g := architecture.NewDAG("test")
	g.AddEdge("z", "u")
	g.AddEdge("z", "v")
	g.AddEdge("z", "x")
	g.AddEdge("v", "w")
	g.AddEdge("w", "u")
	g.AddEdge("u", "x")

	g = g.Invert()

	require.Equal(t, 5, len(g.Nodes()))
	require.Equal(t, 6, len(g.Edges()))

	sub := subgraphTo(t, g, "z")
	require.Equal(t, 5, len(g.Nodes()))
	require.Equal(t, 6, len(g.Edges()))

	// All reachable from z
	require.ElementsMatch(t, createNodes("z", "u", "v", "w", "x"), sub.Nodes(), "Expected z, u, v, w, x")

	for _, node := range g.Nodes() {
		require.True(t, sub.HasNode(node.ID))
	}

	for _, edge := range g.Edges() {
		require.True(t, sub.HasEdge(edge.From, edge.To), "Expected edge %s -> %s", edge.From, edge.To)
	}

	order, err := g.TopologicalSort()
	require.NoError(t, err)
	require.Equal(t, createNodes("x", "u", "w", "v", "z"), order)

	order, err = sub.TopologicalSort()
	require.NoError(t, err)
	require.Equal(t, createNodes("x", "u", "w", "v", "z"), order)

	order, err = g.TopologicalSortTo("z")
	require.NoError(t, err)
	require.Equal(t, createNodes("x", "u", "w", "v"), order)
}
