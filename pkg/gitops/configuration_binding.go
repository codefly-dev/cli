package gitops

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/resources"
)

type namespacedName struct {
	namespace string
	name      string
}

type configurationMaps map[namespacedName]map[string]any

func indexConfigurationMaps(documents []manifest) (configurationMaps, error) {
	index := configurationMaps{}
	for _, doc := range documents {
		if doc.group != "" || doc.kind != "ConfigMap" {
			continue
		}
		key := namespacedName{namespace: metadataString(doc.value, "namespace"), name: metadataString(doc.value, "name")}
		if _, exists := index[key]; exists {
			return nil, fmt.Errorf("configuration binding repeats ConfigMap %s/%s", key.namespace, key.name)
		}
		index[key] = mapField(doc.value, "data")
	}
	return index, nil
}

// A container opts into a service's configuration through the existing Core
// runtime identity variable, not its Kubernetes name or its position in the pod.
func (index configurationMaps) service(container map[string]any, namespace string) (string, error) {
	service := ""
	for _, raw := range sliceField(container, "envFrom") {
		entry, ok := raw.(map[string]any)
		if !ok {
			return "", fmt.Errorf("invalid container envFrom entry")
		}
		prefix, _ := entry["prefix"].(string)
		key, applies := strings.CutPrefix(resources.ServicePrefix, prefix)
		if !applies {
			continue
		}
		ref := mapField(entry, "configMapRef")
		name, _ := ref["name"].(string)
		data, exists := index[namespacedName{namespace: namespace, name: name}]
		if !exists {
			// An unresolved source could replace an earlier envFrom binding.
			service = ""
			continue
		}
		if value, exists := data[key]; exists {
			service, _ = value.(string)
		}
	}
	declared := false
	for _, raw := range sliceField(container, "env") {
		entry, ok := raw.(map[string]any)
		if !ok {
			return "", fmt.Errorf("invalid container environment entry")
		}
		if entry["name"] != resources.ServicePrefix {
			continue
		}
		if declared {
			return "", fmt.Errorf("container repeats %s", resources.ServicePrefix)
		}
		declared = true
		service, _ = entry["value"].(string)
		if source := mapField(entry, "valueFrom"); source != nil {
			ref := mapField(source, "configMapKeyRef")
			name, _ := ref["name"].(string)
			key, _ := ref["key"].(string)
			service, _ = index[namespacedName{namespace: namespace, name: name}][key].(string)
		}
	}
	return service, nil
}

// Follow the selected resource graph only. Another environment's ConfigMap must
// never supply the identity used to bind this environment's workload.
func configurationSources(root, environment string) (map[string][]manifest, error) {
	if err := walkRegularFiles(root, func(_, _ string, _ os.FileInfo) error { return nil }); err != nil {
		return nil, err
	}
	owned, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer owned.Close()
	files := map[string][]manifest{}
	visited := map[string]bool{}
	var visit func(string) error
	visit = func(relative string) error {
		if !filepath.IsLocal(relative) {
			return fmt.Errorf("configuration resource %q escapes the owned tree", relative)
		}
		path := filepath.Join(root, relative)
		info, err := owned.Stat(relative)
		if err != nil {
			return err
		}
		if info.IsDir() {
			path, err = kustomizationPath(path)
			if err != nil {
				return err
			}
			relative, err = filepath.Rel(root, path)
			if err != nil {
				return err
			}
		}
		if visited[relative] {
			return nil
		}
		visited[relative] = true
		data, err := owned.ReadFile(relative)
		if err != nil {
			return err
		}
		documents, customization, err := decodeYAML(relative, data)
		if err != nil {
			return err
		}
		if customization == nil {
			files[path] = documents
			return nil
		}
		for _, reference := range customization.references {
			if err := visit(filepath.FromSlash(reference)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(filepath.Join("overlays", environment)); err != nil {
		return nil, err
	}
	return files, nil
}
