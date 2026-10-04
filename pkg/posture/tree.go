package posture

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// manifestExtensions are the files a rendered tree carries manifests in.
var manifestExtensions = map[string]bool{".yaml": true, ".yml": true, ".json": true}

// unitDirectories are the render subdirectories whose next path segment names
// the unit a manifest belongs to.
var unitDirectories = map[string]bool{"services": true, "solutions": true}

// ValidateTree holds every workload manifest under root to the deployed posture.
// It is the entry point for a caller that has a rendered tree on disk rather than
// decoded documents — the dev deployment's scratch render — and it reads the tree
// exactly as it was written, with no Kustomize build: a render's own tree
// validation covers the built overlay output.
//
// defaults names the subject of a manifest whose path does not identify a unit,
// which is the case for a single-unit render whose tree root *is* that unit.
func ValidateTree(root string, defaults Subject, declaration *Declaration) error {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !manifestExtensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return err
	}
	// Sorted, so a tree with more than one violation always reports the same one.
	sort.Strings(paths)
	for _, path := range paths {
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			relative = path
		}
		relative = filepath.ToSlash(relative)
		data, readErr := os.ReadFile(path) //nolint:gosec // a path from walking the tree under validation
		if readErr != nil {
			return fmt.Errorf("read %s: %w", relative, readErr)
		}
		documents, decodeErr := decodeDocuments(data)
		if decodeErr != nil {
			return fmt.Errorf("%s: %w", relative, decodeErr)
		}
		subject := SubjectFromPath(relative, defaults)
		for _, document := range documents {
			if err := Validate(document, subject, relative, declaration); err != nil {
				return err
			}
		}
	}
	return nil
}

// decodeDocuments decodes every YAML document of one file, skipping anything
// that is not a mapping (a Kustomize patch list, an empty document).
func decodeDocuments(data []byte) ([]map[string]any, error) {
	var documents []map[string]any
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for index := 1; ; index++ {
		var value any
		err := decoder.Decode(&value)
		if err == io.EOF {
			return documents, nil
		}
		if err != nil {
			return nil, fmt.Errorf("document %d: decode YAML: %w", index, err)
		}
		if document, ok := value.(map[string]any); ok {
			documents = append(documents, document)
		}
	}
}

// SubjectFromPath names the unit a manifest path belongs to: the segment after
// the last "modules" segment is the module, and the segment after the last
// "services" or "solutions" segment is the unit. A path that identifies neither
// keeps the caller's defaults. A manifest path may carry a "kustomize:" prefix
// and a "#<document>" suffix from the render's own decoding, and both are
// ignored here.
func SubjectFromPath(path string, defaults Subject) Subject {
	clean := strings.TrimPrefix(filepath.ToSlash(path), "kustomize:")
	clean = strings.SplitN(clean, "#", 2)[0]
	subject := defaults
	segments := strings.Split(clean, "/")
	for index, segment := range segments {
		if index+1 >= len(segments) {
			break
		}
		switch {
		case segment == "modules":
			subject.Module = segments[index+1]
		case unitDirectories[segment]:
			subject.Service = segments[index+1]
		}
	}
	return subject
}
