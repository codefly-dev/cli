package posture

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// Document is one manifest of a selected manifest set: an object a cell would
// apply, after Kustomize has built it and after any list wrapper is unwrapped.
type Document struct {
	// Location is where the document came from, for a refusal to name. It may carry
	// a diagnostic annotation, so it is never parsed for identity.
	Location string
	// Subject is the unit this document belongs to, carried explicitly. Deriving it
	// from Location made a service called "store" become "store (Kustomize output)",
	// and a declaration and an allowance for the real service were then both missed:
	// identity and diagnostics are different things and are kept apart.
	Subject Subject
	Value   map[string]any
}

// kustomizationNames are the file names Kustomize accepts for a kustomization.
var kustomizationNames = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

// referenceFields are the kustomization fields that name another file or
// directory this build consumes. Following them is how coverage is decided:
// a base a root already includes must not be built a second time, or a patch
// that removes something would be judged against the unpatched base as well.
var referenceFields = []string{"resources", "bases", "components", "crds", "transformers", "generators"}

// patchFields are the kustomization fields whose entries may name a patch file.
var patchFields = []string{"patches", "patchesStrategicMerge", "patchesJson6902"}

const overlaysDirectory = "overlays"

// SelectManifests is the manifest set a cell would apply for one environment, and
// the only way this package reads a rendered tree. Every render path uses it, so
// no two paths can disagree about what they are checking.
//
// Selection follows the tree's own structure rather than a convention:
//
//   - every kustomization no other kustomization references is built, which means
//     a base is built once, through the overlay that includes it, so a patch that
//     REMOVES something is honoured rather than judged against the unpatched base;
//   - a directory under another environment's overlays/<name> is not part of this
//     environment's manifests and is neither built nor read;
//   - a manifest file no built kustomization consumes is still decoded, so a unit
//     that ships plain manifests is covered rather than silently skipped.
//
// List wrappers are unwrapped recursively, typed ones included, before anything
// looks at a workload.
func SelectManifests(root, environment string) ([]Document, error) {
	tree, err := readTree(root, environment)
	if err != nil {
		return nil, err
	}
	roots := tree.roots()
	if len(roots) == 0 && len(tree.kustomizations) > 0 {
		// Every kustomization is referenced by another: the references form a cycle,
		// so there is no root to build and the selection would be empty. An empty
		// selection is indistinguishable from a conforming tree, which is how a
		// forbidden volume reached a cell behind a cycle. Refuse instead.
		return nil, fmt.Errorf(
			"the rendered tree's Kustomizations reference each other in a cycle (%s), so there is no root to build: "+
				"this render cannot be checked and is refused rather than read as empty",
			strings.Join(tree.directories(), ", "))
	}
	var documents []Document
	for _, directory := range roots {
		built, buildErr := buildKustomization(root, directory)
		if buildErr != nil {
			return nil, buildErr
		}
		documents = append(documents, built...)
	}
	uncovered, err := tree.uncoveredDocuments(root)
	if err != nil {
		return nil, err
	}
	return expand(append(documents, uncovered...)), nil
}

// tree is what one pass over a rendered tree found: its kustomizations, what each
// references, and its manifest files — with everything belonging to another
// environment already excluded.
type tree struct {
	// kustomizations maps a tree-relative directory to the references it makes,
	// as tree-relative paths.
	kustomizations map[string][]string
	// referenced is every path some kustomization names.
	referenced map[string]bool
	// manifests is every manifest file in the tree.
	manifests []string
}

func readTree(root, environment string) (*tree, error) {
	found := &tree{kustomizations: map[string][]string{}, referenced: map[string]bool{}}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if otherEnvironmentOverlay(relative, environment) {
				return filepath.SkipDir
			}
			return nil
		}
		if isKustomization(entry.Name()) {
			references, parseErr := kustomizationReferences(path, filepath.Dir(relative))
			if parseErr != nil {
				return parseErr
			}
			directory := filepath.ToSlash(filepath.Dir(relative))
			found.kustomizations[directory] = references
			for _, reference := range references {
				found.referenced[reference] = true
			}
			return nil
		}
		if manifestExtensions[strings.ToLower(filepath.Ext(entry.Name()))] {
			found.manifests = append(found.manifests, relative)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(found.manifests)
	return found, nil
}

// otherEnvironmentOverlay reports whether a directory belongs to an environment
// other than the one being selected: the segment right after an "overlays"
// segment names the environment that overlay is for.
func otherEnvironmentOverlay(relative, environment string) bool {
	segments := strings.Split(relative, "/")
	for index, segment := range segments {
		if segment == overlaysDirectory && index+1 < len(segments) {
			return segments[index+1] != environment
		}
	}
	return false
}

// roots are the kustomizations to build: the ones nothing else references.
func (found *tree) roots() []string {
	var roots []string
	for directory := range found.kustomizations {
		if !found.referenced[directory] {
			roots = append(roots, directory)
		}
	}
	sort.Strings(roots)
	return roots
}

// uncoveredDocuments decodes every manifest file no built kustomization consumes.
func (found *tree) uncoveredDocuments(root string) ([]Document, error) {
	covered := map[string]bool{}
	for directory, references := range found.kustomizations {
		covered[directory] = true
		for _, reference := range references {
			covered[reference] = true
		}
	}
	var documents []Document
	for _, relative := range found.manifests {
		if covered[relative] || covered[filepath.ToSlash(filepath.Dir(relative))] || relative == inventoryFilename {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative))) //nolint:gosec // a path from walking the tree under validation
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", relative, err)
		}
		values, decodeErr := decodeDocuments(data)
		if decodeErr != nil {
			return nil, fmt.Errorf("%s: %w", relative, decodeErr)
		}
		subject := SubjectFromPath(relative, Subject{})
		for _, value := range values {
			documents = append(documents, Document{Location: relative, Subject: subject, Value: value})
		}
	}
	return documents, nil
}

// directories lists the kustomization directories of a tree, for a refusal to name.
func (found *tree) directories() []string {
	names := make([]string, 0, len(found.kustomizations))
	for directory := range found.kustomizations {
		names = append(names, directory)
	}
	sort.Strings(names)
	return names
}

// inventoryFilename is the render record a rendered tree carries beside its
// manifests. It is not a manifest.
const inventoryFilename = ".codefly-render.json"

func isKustomization(name string) bool {
	for _, candidate := range kustomizationNames {
		if name == candidate {
			return true
		}
	}
	return false
}

// kustomizationReferences reads the paths one kustomization names, as
// tree-relative paths. A reference outside the tree is kept as given: it is not a
// path this selector can cover, and naming it is better than dropping it.
func kustomizationReferences(path, directory string) ([]string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // a kustomization found by walking the tree under validation
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("%s: decode Kustomization: %w", path, err)
	}
	var references []string
	add := func(value any) {
		text, ok := value.(string)
		if !ok || text == "" || strings.Contains(text, "://") {
			return
		}
		references = append(references, filepath.ToSlash(filepath.Join(directory, text)))
	}
	for _, field := range referenceFields {
		entries, _ := document[field].([]any)
		for _, entry := range entries {
			add(entry)
		}
	}
	for _, field := range patchFields {
		entries, _ := document[field].([]any)
		for _, entry := range entries {
			switch typed := entry.(type) {
			case string:
				add(typed)
			case map[string]any:
				add(typed["path"])
			}
		}
	}
	return references, nil
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
	values, err := decodeDocuments(output)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", directory, err)
	}
	location := directory + " (Kustomize output)"
	subject := SubjectFromPath(directory, Subject{})
	documents := make([]Document, 0, len(values))
	for _, value := range values {
		documents = append(documents, Document{Location: location, Subject: subject, Value: value})
	}
	return documents, nil
}

// expand unwraps every list representation, recursively: a workload inside a
// List, a DeploymentList, or a List of Lists is applied exactly as a top-level
// workload is, so it is read exactly as one.
func expand(documents []Document) []Document {
	flattened := make([]Document, 0, len(documents))
	for _, document := range documents {
		items, isList := listItems(document.Value)
		if !isList {
			flattened = append(flattened, document)
			continue
		}
		nested := make([]Document, 0, len(items))
		for index, item := range items {
			value, ok := item.(map[string]any)
			if !ok {
				continue
			}
			nested = append(nested, Document{
				Location: fmt.Sprintf("%s (items[%d])", document.Location, index),
				Subject:  document.Subject,
				Value:    value,
			})
		}
		flattened = append(flattened, expand(nested)...)
	}
	return flattened
}

// listItems reports the items a document wraps. Every Kubernetes list kind ends
// in "List" and carries its objects under items, so the shape is recognised
// rather than enumerated: a typed DeploymentList hides a workload exactly as a
// plain List does.
func listItems(document map[string]any) ([]any, bool) {
	kind := stringAt(document, "kind")
	if kind == "" || !strings.HasSuffix(kind, "List") {
		return nil, false
	}
	items, ok := document["items"].([]any)
	return items, ok
}

// ValidateRenderedManifests holds already-built manifest text to the deployed
// posture, for a caller that has the Kustomize output in hand rather than a tree
// to select from — the dry-run apply path. It unwraps lists exactly as the
// selector does, so the two paths cannot diverge on what a workload is.
//
// deployed is the environment's own classification: a render that does not target
// a cell is not held to the posture.
func ValidateRenderedManifests(
	manifests, location string,
	defaults Subject,
	contracts Contracts,
	declaration *Declaration,
	deployed bool,
) error {
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
	return ValidateDocuments(expand(documents), defaults, contracts, declaration)
}
