package posture

import (
	"fmt"
	"sort"
)

// referenceIndex answers the references a container's configuration makes into
// the manifest set it is rendered with: the ConfigMaps of that set, by name.
//
// A Secret is deliberately not in it. A restricted render carries no Secret
// values at all, so a value delivered through one cannot be read here — and a
// rule that treated "cannot read" as "conforms" would be satisfied by moving the
// value into a Secret. Such a reference is reported unresolved instead, and a
// rule fails closed on the names it cares about.
type referenceIndex struct {
	configMaps map[string]map[string]string
}

// newReferenceIndex indexes every ConfigMap of an effective manifest set.
func newReferenceIndex(documents []Document) *referenceIndex {
	index := &referenceIndex{configMaps: map[string]map[string]string{}}
	for _, document := range documents {
		if stringAt(document.Value, "kind") != "ConfigMap" {
			continue
		}
		name := metadataName(document.Value)
		if name == "" {
			continue
		}
		entries := map[string]string{}
		for _, field := range []string{"data", "binaryData"} {
			values, _ := document.Value[field].(map[string]any)
			for key, value := range values {
				if text, ok := value.(string); ok {
					entries[key] = text
				}
			}
		}
		index.configMaps[name] = entries
	}
	return index
}

// environmentSource is one envFrom entry.
type environmentSource struct {
	kind     string // "configMap" or "secret"
	name     string
	optional bool
}

func (source environmentSource) describe() string {
	return fmt.Sprintf("%s %q", source.kind, source.name)
}

// environmentSources decodes a container's envFrom list.
func (container pathedContainer) environmentSources() []environmentSource {
	list, ok := container.value["envFrom"].([]any)
	if !ok {
		return nil
	}
	var sources []environmentSource
	for _, entry := range list {
		value, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		for field, kind := range map[string]string{"configMapRef": configMapKind, "secretRef": "secret"} {
			reference, ok := value[field].(map[string]any)
			if !ok {
				continue
			}
			optional, _ := reference["optional"].(bool)
			sources = append(sources, environmentSource{
				kind: kind, name: stringAt(reference, "name"), optional: optional,
			})
		}
	}
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].kind != sources[j].kind {
			return sources[i].kind < sources[j].kind
		}
		return sources[i].name < sources[j].name
	})
	return sources
}

// sourceEntries resolves an envFrom source to the entries it contributes,
// reporting whether the manifest set could answer it.
func (index *referenceIndex) sourceEntries(source environmentSource) (map[string]string, bool) {
	if source.kind != configMapKind {
		return nil, false
	}
	entries, known := index.configMaps[source.name]
	return entries, known
}

// resolveEnvironmentValue reads one env entry: its literal value, or the value a
// ConfigMap in the same set holds for the key it names. Anything else — a Secret
// key, a field or resource reference — is unresolved, named by what it points at.
func resolveEnvironmentValue(entry environmentEntry, index *referenceIndex) (string, bool, string) {
	if entry.hasLiteral {
		return entry.value, true, ""
	}
	if entry.reference == nil {
		// No value and no reference: an entry that carries nothing.
		return "", true, ""
	}
	if entry.reference.kind == configMapKind {
		if entries, known := index.configMaps[entry.reference.name]; known {
			if value, exists := entries[entry.reference.key]; exists {
				return value, true, ""
			}
			return "", true, ""
		}
	}
	return "", false, entry.reference.describe()
}

// valueReference is where an env entry's value comes from when it is not a
// literal.
type valueReference struct {
	kind string // "configMap", "secret", "field", "resource"
	name string
	key  string
}

func (reference valueReference) describe() string {
	if reference.key == "" {
		return fmt.Sprintf("%s %q", reference.kind, reference.name)
	}
	return fmt.Sprintf("%s %q key %q", reference.kind, reference.name, reference.key)
}

func sortStrings(values []string) { sort.Strings(values) }
