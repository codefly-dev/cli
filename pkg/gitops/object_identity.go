package gitops

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Versions do not distinguish Kubernetes objects. Namespace defaulting happens
// before comparison, just as it does when the overlay is applied.
type objectIdentity struct {
	group, kind, namespace, name string
}

func (doc manifest) identity(targetNamespace string) objectIdentity {
	namespace := metadataString(doc.value, keyNamespace)
	if namespace == "" {
		namespace = targetNamespace
		if doc.namespace != nil {
			namespace = *doc.namespace
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
		if documents[i].namespace == nil {
			documents[i].namespace = &namespace
		}
		doc := documents[i]
		id := doc.identity(namespace)
		identities := []objectIdentity{id}
		// Namespace-free output may be cluster-scoped. Index it globally as
		// well as at its applying destination, without a brittle kind roster.
		// A known cluster-scoped object is global even when a namespace
		// transformer that did not know its scope stamped one on it.
		if (metadataString(doc.value, keyNamespace) == "" || doc.knownClusterScoped()) && id.namespace != "" {
			global := id
			global.namespace = ""
			identities = append(identities, global)
		}
		for _, identity := range identities {
			if previous, exists := seen[identity]; exists {
				return fmt.Errorf("%s must occur exactly once in the promoted tree: produced by both %s and %s", identity, previous, doc.path)
			}
			seen[identity] = doc.path
		}
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
