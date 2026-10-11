package gitops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
)

// Versions do not distinguish Kubernetes objects. Namespace defaulting happens
// before comparison, just as it does when the overlay is applied.
type objectIdentity struct {
	group, kind, namespace, name string
}

func (doc manifest) identity(targetNamespace string) objectIdentity {
	namespace := metadataString(doc.value, keyNamespace)
	if _, clusterScoped := clusterScopedKinds[doc.kind]; clusterScoped {
		namespace = ""
	} else if namespace == "" {
		namespace = doc.namespace
		if namespace == "" {
			namespace = targetNamespace
		}
	}
	return objectIdentity{doc.group, doc.kind, namespace, metadataString(doc.value, keyName)}
}

func (id objectIdentity) String() string {
	return fmt.Sprintf("%s %s/%s (apiGroup %q)", id.kind, id.namespace, id.name, id.group)
}

func validateObjectIdentities(documents []manifest, namespace string) error {
	seen := make(map[objectIdentity]string, len(documents))
	for i := range documents {
		if documents[i].namespace == "" {
			documents[i].namespace = namespace
		}
		doc := documents[i]
		id := doc.identity(namespace)
		if previous, exists := seen[id]; exists {
			return fmt.Errorf("%s must occur exactly once in the promoted tree: produced by both %s and %s", id, previous, doc.path)
		}
		seen[id] = doc.path
	}
	return nil
}

// Compare complete effective objects, with deterministic encoding so formatting
// of their source YAML is immaterial. Identity defaulting does not alter these
// bytes: an enclosing transform may not rewrite a service's own output.
func sameObjectBytes(left, right manifest) (bool, error) {
	a, err := json.Marshal(left.value)
	if err != nil {
		return false, err
	}
	b, err := json.Marshal(right.value)
	return bytes.Equal(a, b), err
}

// An Application still applies its overlay when another overlay references it.
// Build both; shared resources must not silently disappear from the inventory.
func appliedOverlay(directory string, opts *RenderOptions) bool {
	if opts.Environment != "" && filepath.Base(directory) == opts.Environment && filepath.Base(filepath.Dir(directory)) == overlaysDirectory {
		return true
	}
	for _, unit := range opts.Units {
		if unit.Path != "" && directory == filepath.ToSlash(filepath.Join(unit.Path, "overlays", opts.Environment)) {
			return true
		}
	}
	for i := range opts.workloadIdentityChecks {
		check := &opts.workloadIdentityChecks[i]
		if directory == filepath.ToSlash(filepath.Join(check.path, "overlays", check.environment.Name)) {
			return true
		}
	}
	return false
}
