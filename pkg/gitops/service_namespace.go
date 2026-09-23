package gitops

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// kustomizationFileNames are the file names kustomize accepts for a
// kustomization, lowercased.
var kustomizationFileNames = []string{kustomizationFile, kustomizationFileAlt, "kustomization"}

// A module's namespace is shared by every service in it, and the CLI's own Argo
// contract already says the render does not create it: writeArgoApplicationSet
// stamps every Application with `CreateNamespace=false`, and
// orchestration.resolveClusterValidation tells the operator to create the
// namespace before a render can be validated against a cluster. The destination
// namespace is therefore provisioned outside the render by construction — by
// the cell's own infrastructure, by an operator, or by the module's authored
// namespace-level layer, which has its own sync wave (moduleResourcesWave) for
// exactly this.
//
// A *service* unit shipping that Namespace object is a category error on three
// counts. It claims a cluster-scoped resource the service does not own; were two
// services in one module to ship it, two Argo Applications would claim the same
// object; and it forces the AppProject to grant cluster-scope authority the
// render otherwise needs for nothing, which a governed cell — whose AppProject
// has an empty clusterResourceWhitelist — refuses outright with
// "resource :Namespace is not permitted in project <name>".
//
// Every canonical service agent already elides it under a restricted profile
// (a `{{ if not .Restricted }}` guard around its namespace.yaml template).
// Relying on each agent to remember is what produced the defect this guards: one
// agent whose template never adopted the guard made its Applications unsyncable
// while every other service in the same cell rendered correctly. Holding the
// staged tree to the contract here makes the render independent of which agent
// version built which service, so a namespace cannot be silently claimed again.
//
// A solution is deliberately untouched: it renders under solutionUnitDir and
// owns its own namespace (see RenderSolution and the AppProject its publish
// generates), which no other unit shares.
//
// The elision is exact. Only a `v1` Namespace named for the very namespace this
// render deploys into is dropped; a Namespace naming anything else is a genuine
// cluster-level claim and is left in the tree for validateManifest to judge
// against the AppProject contract, unchanged.

// elideProvisionedNamespaces removes, from every staged service-unit tree under
// root, the Namespace manifests claiming the namespace the render deploys into,
// and drops the matching entries from the kustomizations that referenced them.
// It returns the tree-relative paths it removed, so the render can report them
// rather than change its committed output silently.
func elideProvisionedNamespaces(root string, opts *RenderOptions) ([]string, error) {
	if !opts.Promotable || opts.Namespace == "" {
		return nil, nil
	}
	units, err := serviceUnitTrees(root)
	if err != nil {
		return nil, err
	}
	var elided []string
	for _, unit := range units {
		dropped, err := elideProvisionedNamespace(filepath.Join(root, unit), opts.Namespace)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", unit, err)
		}
		for _, path := range dropped {
			elided = append(elided, filepath.ToSlash(filepath.Join(unit, path)))
		}
	}
	sort.Strings(elided)
	return elided, nil
}

// serviceUnitTrees lists the service-unit directories inside a staged render,
// tree-relative. A service unit is a directory whose parent is serviceUnitDir —
// the one segment both destination builders in this package join
// (moduleStageDestinations, serviceRenderDestinations), so a module render's
// "services/<name>" and a single-service render's "modules/<module>/services/<name>"
// are both found without either layout being restated here.
func serviceUnitTrees(root string) ([]string, error) {
	var units []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() || path == root {
			return nil
		}
		if filepath.Base(filepath.Dir(path)) != serviceUnitDir {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		units = append(units, relative)
		return filepath.SkipDir
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(units)
	return units, nil
}

// elideProvisionedNamespace removes, from one staged service-unit tree, every
// Namespace manifest claiming namespace, and drops the matching entry from the
// kustomization that referenced the file. It returns the unit-relative paths it
// removed.
func elideProvisionedNamespace(root, namespace string) ([]string, error) {
	if namespace == "" {
		return nil, nil
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		// A managed or otherwise unrendered unit has no staged tree.
		return nil, nil //nolint:nilerr // absence is not a failure here
	}
	var elided []string
	removedByDirectory := make(map[string][]string)
	err := walkRegularFiles(root, func(path, relative string, _ os.FileInfo) error {
		if !isManifestFile(relative) || isKustomizationFile(relative) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", relative, err)
		}
		kept, dropped, err := withoutNamespaceDocuments(relative, data, namespace)
		if err != nil {
			return err
		}
		if dropped == 0 {
			return nil
		}
		elided = append(elided, filepath.ToSlash(relative))
		if len(kept) == 0 {
			if removeErr := os.Remove(path); removeErr != nil {
				return fmt.Errorf("remove %s: %w", relative, removeErr)
			}
			directory := filepath.Dir(path)
			removedByDirectory[directory] = append(removedByDirectory[directory], filepath.Base(path))
			return nil
		}
		encoded, err := encodeDocuments(kept)
		if err != nil {
			return fmt.Errorf("re-encode %s: %w", relative, err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil { //nolint:gosec // manifests are world-readable by design
			return fmt.Errorf("write %s: %w", relative, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for directory, names := range removedByDirectory {
		if err := dropKustomizationResources(directory, names); err != nil {
			return nil, err
		}
	}
	sort.Strings(elided)
	return elided, nil
}

func isManifestFile(relative string) bool {
	switch strings.ToLower(filepath.Ext(relative)) {
	case yamlExtension, ymlExtension:
		return true
	default:
		return false
	}
}

func isKustomizationFile(relative string) bool {
	name := strings.ToLower(filepath.Base(relative))
	for _, candidate := range kustomizationFileNames {
		if name == candidate {
			return true
		}
	}
	return false
}

// withoutNamespaceDocuments splits a manifest file into the documents that stay
// and counts the ones that claim namespace. A document is dropped only when it
// is exactly the `v1` Namespace named namespace.
func withoutNamespaceDocuments(path string, data []byte, namespace string) ([]*yaml.Node, int, error) {
	var kept []*yaml.Node
	dropped := 0
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for document := 1; ; document++ {
		var node yaml.Node
		err := decoder.Decode(&node)
		if err != nil {
			if strings.Contains(err.Error(), "EOF") {
				break
			}
			return nil, 0, fmt.Errorf("%s document %d: decode YAML: %w", path, document, err)
		}
		if isTargetNamespaceDocument(&node, namespace) {
			dropped++
			continue
		}
		kept = append(kept, &node)
	}
	return kept, dropped, nil
}

func isTargetNamespaceDocument(node *yaml.Node, namespace string) bool {
	var value map[string]any
	if err := node.Decode(&value); err != nil {
		return false
	}
	apiVersion, _ := value["apiVersion"].(string)
	kind, _ := value["kind"].(string)
	if apiVersion != "v1" || kind != kindNamespace {
		return false
	}
	return metadataString(value, "name") == namespace
}

func encodeDocuments(documents []*yaml.Node) ([]byte, error) {
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	for _, document := range documents {
		if err := encoder.Encode(document); err != nil {
			return nil, err
		}
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// dropKustomizationResources removes names from the resource lists of the
// kustomization in directory. A kustomization still referencing a file the
// elision removed would fail `kustomize build`, which is how the render checks
// its own promotable output — so the two edits are one operation.
func dropKustomizationResources(directory string, names []string) error {
	removed := make(map[string]struct{}, len(names))
	for _, name := range names {
		removed[name] = struct{}{}
		removed["./"+name] = struct{}{}
	}
	for _, candidate := range kustomizationFileNames {
		path := filepath.Join(directory, candidate)
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		var document yaml.Node
		if decodeErr := yaml.Unmarshal(data, &document); decodeErr != nil {
			return fmt.Errorf("decode %s: %w", path, decodeErr)
		}
		if len(document.Content) == 0 {
			continue
		}
		if !pruneSequenceEntries(document.Content[0], removed) {
			continue
		}
		encoded, err := encodeDocuments([]*yaml.Node{&document})
		if err != nil {
			return fmt.Errorf("re-encode %s: %w", path, err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil { //nolint:gosec // manifests are world-readable by design
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// pruneSequenceEntries drops the named entries from a kustomization's file-list
// keys, reporting whether anything changed.
func pruneSequenceEntries(mapping *yaml.Node, removed map[string]struct{}) bool {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return false
	}
	changed := false
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if !slices.Contains(kustomizeFileListKeys, mapping.Content[index].Value) {
			continue
		}
		sequence := mapping.Content[index+1]
		if sequence.Kind != yaml.SequenceNode {
			continue
		}
		kept := make([]*yaml.Node, 0, len(sequence.Content))
		for _, entry := range sequence.Content {
			if _, drop := removed[entry.Value]; drop {
				changed = true
				continue
			}
			kept = append(kept, entry)
		}
		sequence.Content = kept
	}
	return changed
}
