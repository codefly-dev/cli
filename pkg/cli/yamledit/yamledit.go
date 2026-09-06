// Package yamledit provides node-preserving edits of YAML documents: parse a
// file into a *yaml.Node tree, replace or add individual keys, and re-emit it
// with comments and unmodeled fields intact. It exists so a command can update
// a few fields of a hand-maintained manifest (e.g. one environment in
// workspace.codefly.yaml) without a struct round-trip, which would drop every
// comment and every key the struct does not model.
package yamledit

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Document parses YAML content and returns the document node together with its
// top-level mapping node. An empty document or a non-mapping root is an error,
// since every manifest this package edits is a mapping.
func Document(content []byte) (*yaml.Node, *yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil, fmt.Errorf("empty document")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("document root is not a mapping")
	}
	return &doc, root, nil
}

// MapValue returns the value node for key in a mapping node, or nil when the
// key is absent or the node is not a mapping.
func MapValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// SetMapValue sets key to value in mapping node. An existing key keeps its
// position and its key-node comments; only its value node is swapped. A new key
// is appended with a freshly created key node.
func SetMapValue(node *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content[i+1] = value
			return
		}
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value)
}

// MapKeys returns the keys of a mapping node in document order.
func MapKeys(node *yaml.Node) []string {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	keys := make([]string, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		keys = append(keys, node.Content[i].Value)
	}
	return keys
}

// EndLine returns the last 1-based source line the node's subtree occupies —
// the maximum Line over the node and all its descendants. It lets a caller
// splice exactly the text a node spans back into the original document instead
// of re-serializing the whole file. It does not account for a trailing
// FootComment (which has no node of its own); callers that re-render a node
// must clear its foot comments so the preserved original copy is not
// duplicated.
func EndLine(node *yaml.Node) int {
	end := node.Line
	for _, child := range node.Content {
		if e := EndLine(child); e > end {
			end = e
		}
	}
	return end
}

// EnsureMap returns the mapping value node for key, creating an empty mapping
// and setting it when the key is absent (or holds a non-mapping value).
func EnsureMap(node *yaml.Node, key string) *yaml.Node {
	if existing := MapValue(node, key); existing != nil && existing.Kind == yaml.MappingNode {
		return existing
	}
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	SetMapValue(node, key, m)
	return m
}

// Scalar builds a string scalar node.
func Scalar(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

// Encode builds a yaml.Node from a Go value, matching how yaml.Marshal would
// render it.
func Encode(value any) (*yaml.Node, error) {
	var node yaml.Node
	if err := node.Encode(value); err != nil {
		return nil, err
	}
	return &node, nil
}

// Marshal renders a document node with the 4-space indentation core's
// yaml.Marshal emits, so an edited manifest keeps the project's canonical style.
func Marshal(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(4)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
