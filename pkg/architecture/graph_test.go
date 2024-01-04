package architecture_test

import (
	"testing"

	"github.com/codefly-dev/core/architecture"
	"github.com/stretchr/testify/assert"
)

func TestGraph(t *testing.T) {
	g := architecture.NewGraph("test")
	g.AddNode("a")
	g.AddNode("b")
	g.AddNode("c")
	g.AddEdge("b", "a") // b -> a ...b depends on a
	g.AddEdge("c", "b") // c -> b ...c depends on b
	g.AddNode("d")
	g.AddNode("e")
	g.AddNode("f")
	g.AddEdge("e", "d") // e -> d ...e depends on d
	g.AddEdge("f", "d") // f -> d ...f depends on d
	// a <- b <- c
	// d <- e
	//   <- f

	sub := g.Subgraph("a")
	assert.Equal(t, 3, len(sub.Nodes()))

	sub = g.Subgraph("b")
	assert.Equal(t, 2, len(sub.Nodes()))

	sub = g.Subgraph("c")
	assert.Equal(t, 1, len(sub.Nodes()))

	sub = g.Subgraph("d")
	assert.Equal(t, 3, len(sub.Nodes()))

	// We want the direct antecedents nodes
	antecedents := g.Antecedents("a")
	assert.Equal(t, 1, len(antecedents)) // b
	assert.Contains(t, antecedents, "b")

	antecedents = g.Antecedents("d")
	assert.Equal(t, 2, len(antecedents)) // e, f
	assert.Contains(t, antecedents, "e")
	assert.Contains(t, antecedents, "f")
}
