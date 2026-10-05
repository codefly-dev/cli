package posture

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// Document is one manifest of an effective manifest set: the object a cell would
// apply, after Kustomize has built the environment's overlay and after any
// wrapper list has been expanded.
type Document struct {
	// Location is where the document came from, for a refusal to name.
	Location string
	Value    map[string]any
}

// kustomizationNames are the file names Kustomize accepts for a kustomization.
var kustomizationNames = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

// listKinds are the wrapper kinds whose items are the real objects. A workload
// inside one is applied exactly as a top-level workload is, so the posture must
// see through the wrapper rather than past it.
var listKinds = map[string]bool{"List": true, "ConfigMapList": true, "SecretList": true}

// EffectiveTree is the manifest set a cell would apply for one environment,
// built from a rendered tree on disk.
//
// It selects the requested environment and nothing else: for every unit that has
// an overlays/<environment> directory, that overlay is built with Kustomize, so
// a patch in any form a patch can take (strategic merge, JSON 6902, inline or
// file) is seen as the cell sees it — including a patch that REMOVES something
// the base declared, which is why a tree must never be judged by reading its
// files. Another environment's overlay is not part of this environment's
// manifests and is not read at all.
//
// A unit with no overlay for the environment falls back to its own kustomization,
// and a tree with no kustomization at all to its raw files, so a render that does
// not use Kustomize is still checked.
func EffectiveTree(root, environment string) ([]Document, error) {
	units, err := overlayDirectories(root, environment)
	if err != nil {
		return nil, err
	}
	if len(units) == 0 {
		units, err = rootKustomizations(root)
		if err != nil {
			return nil, err
		}
	}
	if len(units) == 0 {
		return rawDocuments(root)
	}
	var documents []Document
	for _, unit := range units {
		built, buildErr := buildKustomization(root, unit)
		if buildErr != nil {
			return nil, buildErr
		}
		documents = append(documents, built...)
	}
	return documents, nil
}

// overlayDirectories lists the tree-relative overlays/<environment> directories
// that carry a kustomization, in path order.
func overlayDirectories(root, environment string) ([]string, error) {
	if environment == "" {
		return nil, nil
	}
	var selected []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() || entry.Name() != environment {
			return nil
		}
		if filepath.Base(filepath.Dir(path)) != "overlays" || !hasKustomization(path) {
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		selected = append(selected, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(selected)
	return selected, nil
}

// rootKustomizations lists the kustomizations no other kustomization references,
// for a tree that carries no overlay for the environment.
func rootKustomizations(root string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() || !hasKustomization(path) {
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		found = append(found, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(found)
	// A kustomization inside another one's directory tree is built through it.
	var roots []string
	for _, candidate := range found {
		nested := false
		for _, other := range found {
			if other != candidate && strings.HasPrefix(candidate, other+"/") {
				nested = true
				break
			}
		}
		if !nested {
			roots = append(roots, candidate)
		}
	}
	return roots, nil
}

func hasKustomization(directory string) bool {
	for _, name := range kustomizationNames {
		if info, err := os.Stat(filepath.Join(directory, name)); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

// buildKustomization builds one directory and decodes its output.
func buildKustomization(root, directory string) ([]Document, error) {
	kustomizer := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	built, err := kustomizer.Run(filesys.MakeFsOnDisk(), filepath.Join(root, filepath.FromSlash(directory)))
	if err != nil {
		return nil, fmt.Errorf("%s: build Kustomize output: %w", directory, err)
	}
	output, err := built.AsYaml()
	if err != nil {
		return nil, fmt.Errorf("%s: encode Kustomize output: %w", directory, err)
	}
	decoded, err := decodeDocuments(output)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", directory, err)
	}
	location := directory + " (Kustomize output)"
	documents := make([]Document, 0, len(decoded))
	for _, value := range decoded {
		documents = append(documents, Document{Location: location, Value: value})
	}
	return documents, nil
}

// rawDocuments decodes every manifest file under root, for a tree no
// kustomization covers.
func rawDocuments(root string) ([]Document, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && manifestExtensions[strings.ToLower(filepath.Ext(path))] {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var documents []Document
	for _, path := range paths {
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			relative = path
		}
		relative = filepath.ToSlash(relative)
		data, readErr := os.ReadFile(path) //nolint:gosec // a path from walking the tree under validation
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", relative, readErr)
		}
		decoded, decodeErr := decodeDocuments(data)
		if decodeErr != nil {
			return nil, fmt.Errorf("%s: %w", relative, decodeErr)
		}
		for _, value := range decoded {
			documents = append(documents, Document{Location: relative, Value: value})
		}
	}
	return documents, nil
}

// expand flattens the wrapper lists in a document set, so a workload shipped
// inside a List is held to the same rules as one shipped on its own. It is
// applied to every set the posture inspects, wherever that set came from.
func expand(documents []Document) []Document {
	flattened := make([]Document, 0, len(documents))
	for _, document := range documents {
		if !listKinds[stringAt(document.Value, "kind")] {
			flattened = append(flattened, document)
			continue
		}
		items, _ := document.Value["items"].([]any)
		for index, item := range items {
			value, ok := item.(map[string]any)
			if !ok {
				continue
			}
			flattened = append(flattened, Document{
				Location: fmt.Sprintf("%s (items[%d])", document.Location, index),
				Value:    value,
			})
		}
	}
	return flattened
}

// ValidateRenderedManifests holds already-built manifest text to the deployed
// posture. It is for a caller that has the Kustomize output in hand rather than a
// tree on disk — a direct or dry-run apply path — so that every restricted render
// reaches the same rules through the same call rather than through its own reading
// of a tree.
//
// deployed is the environment's own classification: a render that does not target
// a cell is not held to the posture.
func ValidateRenderedManifests(manifests, location string, defaults Subject, declaration *Declaration, deployed bool) error {
	if !deployed {
		return nil
	}
	values, err := decodeDocuments([]byte(manifests))
	if err != nil {
		return fmt.Errorf("%s: %w", location, err)
	}
	documents := make([]Document, 0, len(values))
	for _, value := range values {
		documents = append(documents, Document{Location: location + " (Kustomize output)", Value: value})
	}
	return ValidateDocuments(documents, defaults, declaration)
}
