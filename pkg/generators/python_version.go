package generators

import (
	"fmt"
	"slices"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// locatePythonPackageVersion uses TOML's semantic keys and source ranges to
// replace only project.version. Decoding first rejects invalid/duplicate tables;
// the AST retains comments, quoting and unrelated bytes instead of reformatting
// a manifest a developer owns. Strings containing apparent TOML stay strings.
func locatePythonPackageVersion(path string, data []byte) (int, int, error) {
	if _, err := PythonProjectVersion(path, data); err != nil {
		return 0, 0, err
	}
	var parser unstable.Parser
	parser.Reset(data)
	var table []string
	for parser.NextExpression() {
		node := parser.Expression()
		switch node.Kind {
		case unstable.Table, unstable.ArrayTable:
			table = tomlKeyPath(nil, node)
		case unstable.KeyValue:
			if span, ok := pythonVersionRange(node, table); ok {
				return int(span.Offset), int(span.Offset + span.Length), nil
			}
		}
	}
	if err := parser.Error(); err != nil {
		return 0, 0, fmt.Errorf("cannot parse %s: %w", path, err)
	}
	return 0, 0, fmt.Errorf("cannot find a version in [project] in %s", path)
}

// PythonProjectVersion reads the static PEP 621 metadata version used by both
// generated-client stamping and publication. A tool's version is unrelated.
func PythonProjectVersion(path string, data []byte) (string, error) {
	var document map[string]any
	if err := toml.Unmarshal(data, &document); err != nil {
		return "", fmt.Errorf("cannot parse %s: %w", path, err)
	}
	project, _ := document["project"].(map[string]any)
	version, ok := project["version"].(string)
	if !ok {
		return "", fmt.Errorf("cannot find a version string in [project] in %s", path)
	}
	return version, nil
}

func tomlKeyPath(prefix []string, node *unstable.Node) []string {
	path := slices.Clone(prefix)
	keys := node.Key()
	for keys.Next() {
		path = append(path, string(keys.Node().Data))
	}
	return path
}

func pythonVersionRange(node *unstable.Node, table []string) (unstable.Range, bool) {
	path := tomlKeyPath(table, node)
	value := node.Value()
	if slices.Equal(path, []string{"project", "version"}) && value.Kind == unstable.String {
		return value.Raw, true
	}
	// TOML also allows project = { version = "..." }. Only descend into
	// that one table, never unrelated tables or arrays containing versions.
	if slices.Equal(path, []string{"project"}) && value.Kind == unstable.InlineTable {
		children := value.Children()
		for children.Next() {
			if span, ok := pythonVersionRange(children.Node(), path); ok {
				return span, true
			}
		}
	}
	return unstable.Range{}, false
}
