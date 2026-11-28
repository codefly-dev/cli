package gitops

import (
	"fmt"
	"os"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// splitDeliveredDigests extracts renderer-owned stamps from freshly decoded
// manifests. The remaining objects are the inputs to the normal digest calculation.
func splitDeliveredDigests(manifests []manifest) (map[[2]string]string, []podTemplateAnnotation, error) {
	recorded := make(map[[2]string]string)
	for _, item := range manifests {
		if item.group != deploymentGroup || !servingKind(item.kind) {
			continue
		}
		annotations := mapField(mapField(podTemplate(item), keyMetadata), "annotations")
		if raw, ok := annotations[ConfigurationDigestAnnotation]; ok {
			value, valid := raw.(string)
			key := [2]string{item.kind, metadataString(item.value, keyName)}
			if _, duplicate := recorded[key]; !valid || duplicate {
				return nil, nil, fmt.Errorf("ambiguous configuration digest on %s/%s", key[0], key[1])
			}
			recorded[key] = value
			delete(annotations, ConfigurationDigestAnnotation)
		}
	}
	computed, err := configurationDigests(manifests)
	return recorded, computed, err
}

func deliveredConfigurationDigests(manifests []manifest) ([]podTemplateAnnotation, error) {
	recorded, computed, err := splitDeliveredDigests(manifests)
	if err != nil {
		return nil, err
	}
	if len(recorded) != len(computed) {
		return nil, fmt.Errorf("delivered configuration digest count differs from expected workload count")
	}
	for _, stamp := range computed {
		if recorded[[2]string{stamp.kind, stamp.name}] != stamp.value {
			return nil, fmt.Errorf("delivered configuration digest differs for %s/%s", stamp.kind, stamp.name)
		}
	}
	return computed, nil
}

// refreshConfigurationDigest replaces only the exact generated patches from the
// reviewed tree. Additional edits embedded in those patches must not be discarded.
func refreshConfigurationDigest(tree *renderRoot, unitDir, environment string, prior []podTemplateAnnotation) error {
	overlay, err := renderOverlay(tree, unitDir, environment)
	if err != nil {
		return err
	}
	manifests, err := overlayManifestsInTree(tree, overlay)
	if err != nil {
		return err
	}
	recorded, computed, err := splitDeliveredDigests(manifests)
	if err != nil {
		return err
	}
	if len(prior) != len(computed) || len(recorded) != len(prior) {
		return fmt.Errorf("configuration refresh changed the set of stamped workloads")
	}
	if len(prior) == 0 {
		return nil
	}
	var path string
	for _, candidate := range []string{kustomizationFile, kustomizationFileAlt, kindKustomization} {
		if _, statErr := tree.stat(overlay, candidate); statErr == nil {
			if path != "" {
				return fmt.Errorf("ambiguous overlay kustomization")
			}
			path = candidate
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
	}
	if path == "" {
		return fmt.Errorf("configuration refresh requires a rendered kustomization")
	}
	data, err := tree.read(overlay, path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err = yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	patches := sliceField(doc, "patches")
	desired := make(map[[2]string]podTemplateAnnotation, len(computed))
	for _, stamp := range computed {
		desired[[2]string{stamp.kind, stamp.name}] = stamp
	}
	matched := make(map[int]bool)
	changed := false
	for _, old := range prior {
		key := [2]string{old.kind, old.name}
		next, ok := desired[key]
		if !ok || recorded[key] != old.value {
			return fmt.Errorf("configuration stamp changed unexpectedly for %s/%s", old.kind, old.name)
		}
		delete(desired, key)
		expected, patchErr := old.patch()
		if patchErr != nil {
			return patchErr
		}
		index := -1
		for i, patch := range patches {
			if reflect.DeepEqual(patch, expected) {
				if index >= 0 {
					return fmt.Errorf("duplicate configuration digest patch for %s/%s", old.kind, old.name)
				}
				index = i
			}
		}
		if index < 0 {
			return fmt.Errorf("missing generated configuration digest patch for %s/%s", old.kind, old.name)
		}
		matched[index] = true
		if old.value != next.value {
			patches[index], err = next.patch()
			if err != nil {
				return err
			}
			changed = true
		}
	}
	for i, raw := range patches {
		patch, _ := raw.(map[string]any)
		body, _ := patch[keyPatch].(string)
		if !matched[i] && strings.Contains(body, ConfigurationDigestAnnotation) {
			return fmt.Errorf("unexpected additional configuration digest patch")
		}
	}
	if !changed {
		return nil
	}
	encoded, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return tree.write(overlay, path, encoded)
}
