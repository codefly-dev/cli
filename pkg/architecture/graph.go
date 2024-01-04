package architecture

import (
	"fmt"
	"slices"
	"strings"

	observabilityv1 "github.com/codefly-dev/core/generated/go/observability/v1"
)

type Graph struct {
	Name  string
	nodes map[string]bool
	edges map[string][]string

	nodeTypes map[string]any
}

func NewGraph(name string) *Graph {
	return &Graph{
		Name:      name,
		nodes:     make(map[string]bool),
		edges:     make(map[string][]string),
		nodeTypes: make(map[string]any),
	}
}

func (g *Graph) AddNode(u string) {
	g.nodes[u] = true
}

func (g *Graph) AddTypedNode(u string, t any) {
	g.nodes[u] = true
	g.nodeTypes[u] = t
}

func (g *Graph) AddEdge(u, v string) {
	if !g.nodes[u] {
		g.nodes[u] = true
	}
	if !g.nodes[v] {
		g.nodes[v] = true
	}
	if !slices.Contains(g.edges[u], v) {
		g.edges[u] = append(g.edges[u], v)
	}
}

type Node struct {
	ID   string
	Type any
}

func (n Node) String() string {
	return n.ID
}

func (g *Graph) Nodes() []Node {
	var nodes []Node
	for node := range g.nodes {
		nodes = append(nodes, Node{
			ID:   node,
			Type: g.nodeTypes[node],
		})
	}
	return nodes
}

type Edge struct {
	From string
	To   string
}

func (e Edge) String() string {
	return fmt.Sprintf("(%s -> %s)", e.From, e.To)
}

func (g *Graph) Edges() []Edge {
	var edges []Edge
	for from, tos := range g.edges {
		for _, to := range tos {
			edges = append(edges, Edge{
				From: from,
				To:   to,
			})
		}
	}
	return edges
}

func ToType(t any) observabilityv1.GraphNode_Type {
	return observabilityv1.GraphNode_Type(observabilityv1.GraphNode_Type_value[strings.ToUpper(t.(string))])
}

func ToGraphResponse(g *Graph) *observabilityv1.GraphResponse {
	resp := &observabilityv1.GraphResponse{}
	for _, node := range g.Nodes() {
		resp.Nodes = append(resp.Nodes, &observabilityv1.GraphNode{
			Id:   node.ID,
			Type: ToType(node.Type),
		})
	}
	for _, edge := range g.Edges() {
		resp.Edges = append(resp.Edges, &observabilityv1.GraphEdge{
			From: edge.From,
			To:   edge.To,
		})
	}
	return resp
}

func (g *Graph) TopologicalSort() []string {
	visited := make(map[string]bool)
	var stack []string

	var dfs func(node string)

	dfs = func(node string) {
		visited[node] = true
		for _, n := range g.edges[node] {
			if !visited[n] {
				dfs(n)
			}
		}
		stack = append([]string{node}, stack...)
	}

	for node := range g.nodes {
		if !visited[node] {
			dfs(node)
		}
	}
	return stack
}

func (g *Graph) TopologicalSortFrom(node string) []string {
	results := g.Subgraph(node).TopologicalSort()
	// Discard the last one and revert the order
	if len(results) == 1 {
		return nil
	}
	Reverse(results)
	return results[1:]
}

func (g *Graph) Subgraph(start string) *Graph {
	visited := make(map[string]bool)
	subgraph := NewGraph("subgraph")

	// Create a reverse mapping of edges (from child to parent)
	reverseEdges := make(map[string][]string)
	for parent, children := range g.edges {
		for _, child := range children {
			reverseEdges[child] = append(reverseEdges[child], parent)
		}
	}

	var dfs func(node string)
	dfs = func(node string) {
		visited[node] = true
		subgraph.AddNode(node)

		// Iterate over the parents of the current node
		for _, parent := range reverseEdges[node] {
			if !visited[parent] {
				subgraph.AddEdge(parent, node) // Add edge from parent to child
				dfs(parent)
			}
		}
	}

	dfs(start)
	return subgraph
}

func (g *Graph) Antecedents(s string) []string {
	// I really need to invert this graph...
	var res []string
	for node, edges := range g.edges {
		for _, edge := range edges {
			if edge == s {
				res = append(res, node)
			}
		}
	}
	return res
}

func Reverse[T any](ss []T) {
	for i := len(ss)/2 - 1; i >= 0; i-- {
		opp := len(ss) - 1 - i
		ss[i], ss[opp] = ss[opp], ss[i]
	}
}
