package architecture

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	observabilityv0 "github.com/codefly-dev/core/generated/go/observability/v0"
)

type DAG struct {
	Name          string
	nodes         map[string]bool
	edges         map[string][]string
	incomingEdges map[string]int

	nodeTypes map[string]any
	verb      string
}

func NewDAG(name string) *DAG {
	return &DAG{
		Name:          name,
		nodes:         make(map[string]bool),
		edges:         make(map[string][]string),
		incomingEdges: make(map[string]int),
		nodeTypes:     make(map[string]any),
	}
}

func (g *DAG) Verb() string {
	if g.verb == "" {
		return "->"
	}
	return g.verb
}

type WrappedNode struct {
	u string
	g *DAG
}

func (w *WrappedNode) WithType(t any) *WrappedNode {
	w.g.nodeTypes[w.u] = t
	return w
}

func (w *WrappedNode) WithTypeOf(node string) {
	if t, ok := w.g.nodeTypes[node]; ok {
		w.g.nodeTypes[w.u] = t
	}
}

func (g *DAG) AddNode(u string) *WrappedNode {
	g.nodes[u] = true
	return &WrappedNode{
		u: u,
		g: g,
	}
}

func (g *DAG) AddEdge(u, v string) {
	if !g.nodes[u] {
		g.nodes[u] = true
	}
	if !g.nodes[v] {
		g.nodes[v] = true
	}
	if !slices.Contains(g.edges[u], v) {
		g.edges[u] = append(g.edges[u], v)
		g.incomingEdges[v]++
	}
}

type Node struct {
	ID   string
	Type any
}

func (n Node) String() string {
	if n.Type == nil {
		return n.ID
	}
	return fmt.Sprintf("%s (%s)", n.ID, n.Type)
}

func (g *DAG) Node(node string) *Node {
	if !g.HasNode(node) {
		return nil
	}
	return &Node{
		ID:   node,
		Type: g.nodeTypes[node],
	}
}

func (g *DAG) Nodes() []Node {
	var nodes []Node
	for node := range g.nodes {
		n := g.Node(node)
		if n != nil {
			nodes = append(nodes, *n)
		}
	}
	return nodes
}

type Edge struct {
	From string
	To   string
}

func (g *DAG) EdgeString(edge Edge) string {
	return fmt.Sprintf("(%s %s %s)", g.Node(edge.From), g.Verb(), g.Node(edge.To))
}

func (g *DAG) Edges() []Edge {
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

func (g *DAG) NodeFrom(node string) []Node {
	var nodes []Node
	for _, to := range g.edges[node] {
		n := g.Node(to)
		if n != nil {
			nodes = append(nodes, *n)
		}
	}
	return nodes
}

func (g *DAG) WithRelationVerb(verb string) {
	g.verb = verb
}

func (g *DAG) Print() string {
	var res []string
	for _, node := range g.Nodes() {
		// Sort the edges for consistent output
		var elements []string
		for _, to := range g.NodeFrom(node.ID) {
			elements = append(elements, to.String())
		}
		if len(elements) == 0 {
			res = append(res, node.String())
			continue
		}
		sort.Strings(elements)
		res = append(res, fmt.Sprintf("%s %s %s", node, g.Verb(), strings.Join(elements, ", ")))
	}
	return strings.Join(res, "\n")
}

func (g *DAG) PrintAsDot() string {
	var builder strings.Builder
	builder.WriteString("digraph {\n")
	for _, edge := range g.Edges() {
		builder.WriteString(fmt.Sprintf("\t\"%s\" -> \"%s\";\n", edge.From, edge.To))
	}
	builder.WriteString("}\n")
	return builder.String()
}

func (g *DAG) Invert() *DAG {
	inverted := NewDAG(g.Name + "-inverted")
	for _, node := range g.Nodes() {
		inverted.AddNode(node.ID).WithType(g.nodeTypes[node.ID])
	}
	for _, edge := range g.Edges() {
		inverted.AddEdge(edge.To, edge.From)
	}
	return inverted
}

func ToType(t any) observabilityv0.GraphNode_Type {
	return observabilityv0.GraphNode_Type(observabilityv0.GraphNode_Type_value[strings.ToUpper(t.(string))])
}

func ToGraphResponse(g *DAG) *observabilityv0.GraphResponse {
	resp := &observabilityv0.GraphResponse{}
	for _, node := range g.Nodes() {
		resp.Nodes = append(resp.Nodes, &observabilityv0.GraphNode{
			Id:   node.ID,
			Type: ToType(node.Type),
		})
	}
	for _, edge := range g.Edges() {
		resp.Edges = append(resp.Edges, &observabilityv0.GraphEdge{
			From: edge.From,
			To:   edge.To,
		})
	}
	return resp
}

func (g *DAG) TopologicalSort() ([]Node, error) {
	var sorted []string
	var queue []string

	// Find all nodes with no incoming edges
	for node := range g.nodes {
		if g.incomingEdges[node] == 0 {
			queue = append(queue, node)
		}
	}

	for len(queue) > 0 {
		// Sort queue to ensure deterministic output
		sort.Strings(queue)
		node := queue[0]
		queue = queue[1:]

		sorted = append(sorted, node)

		for _, neighbor := range g.edges[node] {
			g.incomingEdges[neighbor]--
			if g.incomingEdges[neighbor] == 0 {
				queue = append(queue, neighbor)
			}
		}
	}

	if len(sorted) != len(g.nodes) {
		// DAG has a cycle
		return nil, fmt.Errorf("graph has a cycle")
	}
	var out []Node
	for _, node := range sorted {
		n := g.Node(node)
		if n != nil {
			out = append(out, *n)
		}
	}
	return out, nil
}

func (g *DAG) TopologicalSortTo(endNode string) ([]Node, error) {
	sub, err := g.SubGraphTo(endNode)
	if err != nil {
		return nil, fmt.Errorf("cannot invert graph: %w", err)
	}
	sub = sub.Invert()
	sorted, err := sub.TopologicalSortFrom(endNode)
	slices.Reverse(sorted)
	return sorted, nil
}

// ReachableFrom returns true if the endNode is reachable from the startNode
func (g *DAG) ReachableFrom(startNode string, endNode string) bool {
	sub, err := g.SubGraphFrom(startNode)
	if err != nil {
		return false
	}
	return sub.HasNode(endNode)
}

func (g *DAG) TopologicalSortFrom(startNode string) ([]Node, error) {
	sub, err := g.SubGraphFrom(startNode)
	if err != nil {
		return nil, fmt.Errorf("cannot invert graph: %w", err)
	}
	sorted, err := sub.TopologicalSort()
	if err != nil {
		return nil, fmt.Errorf("cannot sort graph to startNode %s", startNode)
	}
	if len(sorted) < 2 {
		return nil, nil
	}
	sorted = sorted[1:]
	return sorted, nil
}

// SubGraphFrom returns a subgraph that contains all nodes that are reachable from the given node.
func (g *DAG) SubGraphFrom(startNode string) (*DAG, error) {
	if !g.HasNode(startNode) {
		return nil, fmt.Errorf("cannot create subgraph to node %s: node does not exist", startNode)
	}
	subGraph := NewDAG(fmt.Sprintf("%s-reachable-from-%s", g.Name, startNode))
	subGraph.verb = g.verb
	visited := make(map[string]bool)

	var dfs func(node string)
	dfs = func(node string) {
		visited[node] = true
		subGraph.AddNode(node).WithType(g.nodeTypes[node])
		if g.nodeTypes[node] != nil {
			subGraph.nodeTypes[node] = g.nodeTypes[node]
		}

		for _, child := range g.edges[node] {
			if !visited[child] {
				subGraph.AddNode(child).WithType(g.nodeTypes[child])
				subGraph.AddEdge(node, child)
				dfs(child)
			} else if subGraph.HasNode(child) {
				subGraph.AddEdge(node, child)
			}
		}
	}

	dfs(startNode)

	return subGraph, nil
}

func (g *DAG) SubGraphTo(endNode string) (*DAG, error) {
	if !g.HasNode(endNode) {
		return nil, fmt.Errorf("cannot create subgraph to node %s: node does not exist", endNode)
	}
	subGraph := NewDAG(fmt.Sprintf("%s-reachable-to-%s", g.Name, endNode))
	subGraph.verb = g.verb
	visited := make(map[string]bool)

	var dfs func(node string)
	dfs = func(node string) {
		visited[node] = true
		subGraph.AddNode(node).WithType(g.nodeTypes[node])
		if g.nodeTypes[node] != nil {
			subGraph.nodeTypes[node] = g.nodeTypes[node]
		}

		for _, parent := range g.Parents(node) {
			if !visited[parent.ID] {
				subGraph.AddNode(parent.ID).WithType(g.nodeTypes[parent.ID])
				subGraph.AddEdge(parent.ID, node)
				dfs(parent.ID)
			} else if subGraph.HasNode(parent.ID) {
				subGraph.AddEdge(parent.ID, node)
			}
		}
	}
	dfs(endNode)
	return subGraph, nil
}

func (g *DAG) Parents(s string) []Node {
	var res []Node
	for node, edges := range g.edges {
		for _, edge := range edges {
			if edge == s {
				res = append(res, Node{
					ID:   node,
					Type: g.nodeTypes[node],
				})
			}
		}
	}
	return res
}

func (g *DAG) Children(startNode string) []Node {
	var res []Node
	for _, edge := range g.edges[startNode] {
		res = append(res, Node{
			ID:   edge,
			Type: g.nodeTypes[edge],
		})
	}
	return res
}

func (g *DAG) SortedChildren(startNode string) ([]Node, error) {
	sorted, err := g.TopologicalSortFrom(startNode)
	if err != nil {
		return nil, fmt.Errorf("cannot sort graph to startNode %s", startNode)
	}
	var out []Node
	for _, node := range sorted {
		// Should be part of the edge
		if !slices.Contains(g.edges[startNode], node.ID) {
			continue
		}
		n := g.Node(node.ID)
		if n != nil {
			out = append(out, *n)
		}
	}
	return out, nil
}

func (g *DAG) HasNode(node string) bool {
	return g.nodes[node]
}

func (g *DAG) SortedParents(endNode string) ([]Node, error) {
	sorted, err := g.TopologicalSortTo(endNode)
	if err != nil {
		return nil, fmt.Errorf("cannot sort graph to endNode %s", endNode)
	}
	var out []Node
	for _, node := range sorted {
		// Should be part of the edge
		if !slices.Contains(g.edges[node.ID], endNode) {
			continue
		}
		n := g.Node(node.ID)
		if n != nil {
			out = append(out, *n)
		}
	}
	return out, nil
}

func (g *DAG) HasEdge(from string, to string) bool {
	return slices.Contains(g.edges[from], to)
}
