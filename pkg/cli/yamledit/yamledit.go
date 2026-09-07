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
	"strings"

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
	// A scalar can occupy more than its start line: a literal/double-quoted block
	// scalar keeps its newlines in Value, so each embedded newline is one extra
	// source line. Counting them is exact for newline-preserving styles (|, |-,
	// |+, double-quoted) and never over-counts for folded/plain-wrapped styles
	// (which fold physical newlines into spaces, leaving fewer newlines in Value
	// than lines on disk). Under-counting can only ever duplicate a trailing line
	// on splice, never strand following content — so a scalar the count cannot
	// fully measure still fails visibly rather than deleting bytes. Non-scalar
	// nodes carry an empty Value, so this adds nothing for them.
	end := node.Line + strings.Count(node.Value, "\n")
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

// RenderItem serializes a single sequence item (scalar or mapping) and
// left-pads every line to indent spaces, reproducing the project's canonical
// block-sequence style ("- key: value").
func RenderItem(item *yaml.Node, indent int) ([]string, error) {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{item}}
	b, err := Marshal(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{seq}})
	if err != nil {
		return nil, err
	}
	pad := strings.Repeat(" ", indent)
	raw := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	out := make([]string, len(raw))
	for i, l := range raw {
		if l == "" {
			out[i] = ""
			continue
		}
		out[i] = pad + l
	}
	return out, nil
}

// LineIndent counts the leading spaces of a line.
func LineIndent(line string) int {
	n := 0
	for n < len(line) && line[n] == ' ' {
		n++
	}
	return n
}

// SpliceLines returns lines with [start,end) replaced by repl.
func SpliceLines(lines []string, start, end int, repl []string) []byte {
	out := make([]string, 0, len(lines)-(end-start)+len(repl))
	out = append(out, lines[:start]...)
	out = append(out, repl...)
	out = append(out, lines[end:]...)
	return []byte(strings.Join(out, "\n"))
}

// keyLineIndex returns the 0-based line index of key's own line within
// parent, or the line just past parent's mapping when key is absent
// (unreachable for a caller that only reaches here after confirming key
// exists).
func keyLineIndex(parent *yaml.Node, key string) int {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			return parent.Content[i].Line - 1
		}
	}
	return len(parent.Content)
}

// AppendSequenceItems appends items to key's block sequence under parent (the
// document's top-level mapping node, from Document) in original, preserving
// every other byte: existing items, comments, blank lines, and unrelated keys
// are re-serialized only where an item is actually inserted.
//
// key already holding a non-empty block sequence gets items appended after
// its last element; key present but empty or inline (`key: []`, `key:`) is
// replaced with a fresh block sequence; key entirely absent gets a new
// "key:" section appended at end of file.
func AppendSequenceItems(original []byte, parent *yaml.Node, key string, items []*yaml.Node) ([]byte, error) {
	if len(items) == 0 {
		return original, nil
	}
	lines := strings.Split(string(original), "\n")
	seq := MapValue(parent, key)

	if seq != nil && seq.Kind == yaml.SequenceNode && len(seq.Content) > 0 {
		indent := LineIndent(lines[seq.Content[0].Line-1])
		var rendered []string
		for _, item := range items {
			r, err := RenderItem(item, indent)
			if err != nil {
				return nil, err
			}
			rendered = append(rendered, r...)
		}
		last := seq.Content[len(seq.Content)-1]
		ins := EndLine(last)
		return SpliceLines(lines, ins, ins, rendered), nil
	}

	rendered := []string{key + ":"}
	for _, item := range items {
		r, err := RenderItem(item, 4)
		if err != nil {
			return nil, err
		}
		rendered = append(rendered, r...)
	}
	if seq != nil {
		line := keyLineIndex(parent, key)
		return SpliceLines(lines, line, line+1, rendered), nil
	}
	ins := len(lines)
	if ins > 0 && lines[ins-1] == "" {
		ins--
	}
	return SpliceLines(lines, ins, ins, rendered), nil
}
