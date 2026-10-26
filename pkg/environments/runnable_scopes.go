package environments

import (
	"encoding/json"
	"fmt"
	"sort"

	runnablev0 "github.com/codefly-dev/core/generated/go/codefly/runnable/v0"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"
)

// ScopeSelections carries Core's ScopeSelection messages in the existing
// environment YAML. Proto JSON owns the field vocabulary and refuses unknown
// fields; Core's ResolveScopeSlots owns every authority rule. This adapter only
// changes the serialization, including on the Core workspace load/save path.
//
// An explicitly empty list (`[]`) is a value of its own, distinct from an
// absent key and from a null one: it declines the binding (see
// DeclinesRunnableBinding). The decoder keeps it a non-nil empty slice and the
// encoder writes a nil one as null, so the three spellings survive the
// load/save round trip unchanged.
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
	if s == nil {
		return nil, nil
	}
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

// DeclinesRunnableBinding reports whether the environment answers a binding
// key with an explicitly empty list: this environment installs the operation
// for no resource, so `generate runnable-bindings` prepares no binding for it
// and grants it no authority. Only the empty list declines. An absent key is
// not a decline — a required slot left unanswered is still refused, so nothing
// is ever skipped by omission.
func (env *Environment) DeclinesRunnableBinding(key string) bool {
	selections, present := env.RunnableScopeSelections[key]
	return present && selections != nil && len(selections) == 0
}

// DeclinedRunnableBindings is every binding key the environment declines,
// sorted.
func (env *Environment) DeclinedRunnableBindings() []string {
	var declined []string
	for key := range env.RunnableScopeSelections {
		if env.DeclinesRunnableBinding(key) {
			declined = append(declined, key)
		}
	}
	sort.Strings(declined)
	return declined
}

// validateRunnableScopeSelections refuses a key whose value is null (`KEY:`
// or `KEY: ~`). It would otherwise read as either an absent key or a decline
// depending on who reads it; the two are spelled differently on purpose.
func (env *Environment) validateRunnableScopeSelections() error {
	keys := make([]string, 0, len(env.RunnableScopeSelections))
	for key := range env.RunnableScopeSelections {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if env.RunnableScopeSelections[key] == nil {
			return fmt.Errorf("runnable-scope-selections[%s] is null: list the ScopeSelection messages that answer its slots, or write [] to decline the binding in this environment", key)
		}
	}
	return nil
}

// validateRunnableCallEndpoint refuses a runnable-call-endpoint that is not a
// composition identity. Whether it names a declared HTTP-based endpoint is
// answered by generate runnable-bindings, which resolves it against the
// workspace.
func (env *Environment) validateRunnableCallEndpoint() error {
	if env.RunnableCallEndpoint != "" && !deliveryPattern.MatchString(env.RunnableCallEndpoint) {
		return fmt.Errorf("runnable-call-endpoint %q is not the composition identity <module>/<service>/<endpoint> of a declared HTTP-based endpoint", env.RunnableCallEndpoint)
	}
	return nil
}
