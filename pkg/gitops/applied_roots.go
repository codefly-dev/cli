package gitops

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
)

// An applied root is an inventory layer or an Argo source, never a directory
// that merely happens to have an environment-shaped name. A dependency is built
// with each root that applies it, not independently just because it is shared.
type appliedRoot struct{ directory, namespace string }

func inventoryAppliedRoots(opts *RenderOptions) []appliedRoot {
	inventory := inventoryHead(opts)
	var roots []appliedRoot
	add := func(path, namespace string) {
		if path != "" {
			roots = append(roots, appliedRoot{filepath.ToSlash(filepath.Join(path, overlaysDirectory, opts.Environment)), namespace})
		}
	}
	// A standalone render records its unit in the inventory header instead
	// of the module graph. Its layout is shared with serviceRenderDestinations.
	if inventory.Unit != "" {
		add(serviceRenderPath(inventory.Module, inventory.Unit), opts.Namespace)
	}
	for _, unit := range renderedUnits(&inventory) {
		add(unit.Path, opts.Namespace)
	}
	add(inventory.ModulePath, opts.Namespace)
	add(inventory.ModuleManifestPath, opts.Namespace)
	return roots
}

func applicationRoots(doc manifest, opts *RenderOptions) ([]appliedRoot, error) {
	if doc.group != argoAPIGroup {
		return nil, nil
	}
	spec := mapField(doc.value, keySpec)
	var parameters []map[string]any
	switch doc.kind {
	case kindApplication:
		parameters = []map[string]any{{}}
	case kindApplicationSet:
		parameters = applicationSetParameters(spec["generators"])
		spec = mapField(mapField(spec, "template"), keySpec)
	default:
		return nil, nil
	}
	sources := sliceField(spec, "sources")
	if source := mapField(spec, "source"); source != nil {
		sources = append(sources, source)
	}
	var roots []appliedRoot
	for _, raw := range sources {
		source, _ := raw.(map[string]any)
		sourcePath, _ := source["path"].(string)
		if sourcePath == "" {
			continue
		} // A chart or a values-only source has no local tree.
		destination, _ := mapField(spec, "destination")[keyNamespace].(string)
		var failures []error
		for _, values := range parameters {
			path, err := applicationValue(sourcePath, values)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			namespace, err := applicationValue(destination, values)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			path = filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
			if opts.OwnedPath != "" {
				path = strings.TrimPrefix(path, strings.TrimSuffix(filepath.ToSlash(opts.OwnedPath), "/")+"/")
			}
			if !filepath.IsLocal(filepath.FromSlash(path)) {
				return nil, fmt.Errorf("%s: Application source %q escapes the render", doc.path, path)
			}
			roots = append(roots, appliedRoot{path, namespace})
		}
		if len(failures) != 0 {
			return nil, fmt.Errorf("%s: cannot resolve Application source path/destination: %w", doc.path, failures[0])
		}
	}
	return roots, nil
}

func applicationValue(value string, parameters map[string]any) (string, error) {
	if !strings.Contains(value, "{{") {
		return value, nil
	}
	parsed, err := template.New("application").Option("missingkey=error").Parse(value)
	if err != nil {
		return "", err
	}
	var output bytes.Buffer
	if err = parsed.Execute(&output, parameters); err != nil {
		return "", err
	}
	return output.String(), nil
}

// List elements carry the locally declared source/destination values. Matrix
// generators combine their child parameters; external tenant generators do not
// change the component path or namespace. If those depend on live generator
// data, applicationValue refuses them instead of inventing an applied root.
func applicationSetParameters(value any) []map[string]any {
	generators, _ := value.([]any)
	var result []map[string]any
	for _, raw := range generators {
		generator, _ := raw.(map[string]any)
		if matrix := mapField(generator, "matrix"); matrix != nil {
			rows := []map[string]any{{}}
			for _, child := range sliceField(matrix, "generators") {
				var combined []map[string]any
				for _, left := range rows {
					for _, right := range applicationSetParameters([]any{child}) {
						row := make(map[string]any)
						for key, value := range left {
							row[key] = value
						}
						for key, value := range right {
							row[key] = value
						}
						combined = append(combined, row)
					}
				}
				rows = combined
			}
			result = append(result, rows...)
		} else if list := mapField(generator, "list"); list != nil {
			for _, element := range sliceField(list, "elements") {
				if row, ok := element.(map[string]any); ok {
					result = append(result, row)
				}
			}
		} else {
			result = append(result, map[string]any{})
		}
	}
	if len(result) == 0 {
		return []map[string]any{{}}
	}
	return result
}

func sortedAppliedRoots(roots map[appliedRoot]bool) []appliedRoot {
	result := make([]appliedRoot, 0, len(roots))
	for root := range roots {
		result = append(result, root)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].directory == result[j].directory {
			return result[i].namespace < result[j].namespace
		}
		return result[i].directory < result[j].directory
	})
	return result
}
