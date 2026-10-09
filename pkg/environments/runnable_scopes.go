package environments

import (
	"encoding/json"
	"fmt"

	runnablev0 "github.com/codefly-dev/core/generated/go/codefly/runnable/v0"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"
)

// ScopeSelections carries Core's ScopeSelection messages in the existing
// environment YAML. Proto JSON owns the field vocabulary and refuses unknown
// fields; Core's ResolveScopeSlots owns every authority rule. This adapter only
// changes the serialization, including on the Core workspace load/save path.
type ScopeSelections []*runnablev0.ScopeSelection

func (s *ScopeSelections) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("runnable-scope-selections must contain a list of ScopeSelection messages")
	}
	selections := make(ScopeSelections, 0, len(node.Content))
	for _, item := range node.Content {
		var value map[string]any
		if err := item.Decode(&value); err != nil {
			return err
		}
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		selection := &runnablev0.ScopeSelection{}
		if err := protojson.Unmarshal(data, selection); err != nil {
			return fmt.Errorf("runnable-scope-selections: %w", err)
		}
		selections = append(selections, selection)
	}
	*s = selections
	return nil
}

func (s ScopeSelections) MarshalYAML() (any, error) {
	values := make([]any, 0, len(s))
	for _, selection := range s {
		data, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(selection)
		if err != nil {
			return nil, err
		}
		var value any
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}
