package posture

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// manifestExtensions are the files a rendered tree carries manifests in.
var manifestExtensions = map[string]bool{".yaml": true, ".yml": true, ".json": true}

// unitDirectories are the render subdirectories whose next path segment names
// the unit a manifest belongs to.
var unitDirectories = map[string]bool{"services": true, "solutions": true}

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
