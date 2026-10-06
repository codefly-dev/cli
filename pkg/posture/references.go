package posture

import (
	"fmt"
	"sort"
)

// referenceIndex answers the references a container's configuration makes into the
// manifest set it is rendered with. It is keyed by NAMESPACE AND NAME: two
// ConfigMaps of the same name in different namespaces are two objects, and an index
// keyed by name alone let the second one answer for the first.
//
// A Secret is deliberately absent. A restricted render carries no Secret values, so
// a value delivered through one cannot be read — and a rule that treated "cannot
// read" as "conforms" would be satisfied by moving the value into a Secret. Such a
// reference is reported unresolved, and the rules fail closed on the names that
// matter.
type referenceIndex struct {
	configMaps map[string]map[string]string
}

func indexKey(namespace, name string) string { return namespace + "/" + name }

// newReferenceIndex indexes every ConfigMap of a selected manifest set.
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
		index.configMaps[indexKey(metadataNamespace(document.Value), name)] = entries
	}
	return index
}

// environmentSource is one envFrom entry, with the prefix it applies to every key
// it contributes.
type environmentSource struct {
	kind   string // "configMap" or "secret"
	name   string
	prefix string
}

func (source environmentSource) describe() string {
	if source.prefix == "" {
		return fmt.Sprintf("%s %q", source.kind, source.name)
	}
	return fmt.Sprintf("%s %q with prefix %q", source.kind, source.name, source.prefix)
}

// environmentSources decodes a container's envFrom list, in declaration order:
// Kubernetes lets a later source override an earlier one, so order is meaning.
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
		prefix := stringAt(value, "prefix")
		for field, kind := range map[string]string{"configMapRef": configMapKind, "secretRef": "secret"} {
			reference, ok := value[field].(map[string]any)
			if !ok {
				continue
			}
			sources = append(sources, environmentSource{
				kind: kind, name: stringAt(reference, "name"), prefix: prefix,
			})
		}
	}
	return sources
}

// sourceEntries resolves an envFrom source in the consuming workload's namespace.
func (index *referenceIndex) sourceEntries(source environmentSource, namespace string) (map[string]string, bool) {
	if source.kind != configMapKind {
		return nil, false
	}
	entries, known := index.configMaps[indexKey(namespace, source.name)]
	return entries, known
}

// resolveEnvironmentValue reads one env entry: its literal value, or the value a
// ConfigMap of the same namespace holds for the key it names. A key the ConfigMap
// does not hold is UNRESOLVED, not an empty string: a missing key is something the
// render cannot see, and an empty value would read as conformance.
func resolveEnvironmentValue(entry environmentEntry, index *referenceIndex, namespace string) (string, bool, string) {
	if entry.hasLiteral {
		return entry.value, true, ""
	}
	if entry.reference == nil {
		return "", true, ""
	}
	if entry.reference.kind == configMapKind {
		if entries, known := index.configMaps[indexKey(namespace, entry.reference.name)]; known {
			if value, exists := entries[entry.reference.key]; exists {
				return value, true, ""
			}
		}
	}
	return "", false, entry.reference.describe()
}

// valueReference is where an env entry's value comes from when it is not a literal.
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

func metadataNamespace(document map[string]any) string {
	metadata, ok := document["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	return stringAt(metadata, "namespace")
}

func sortedStrings(values []string) []string {
	sort.Strings(values)
	return values
}
