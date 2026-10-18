package gitops

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const kindService = "Service"

// projectServiceMesh stamps the labels the environment declares under
// `service-mesh` on the Service of each named service this module renders —
// the cell's waypoint attachment on a presenter — as a patch in the unit's
// environment overlay, so the base the agent rendered is untouched. Every
// other Service is left as rendered: attaching one puts all its ports behind
// an L7 hop it was never rendered for.
func projectServiceMesh(tree *renderRoot, owned string, opts *RenderOptions) error {
	if opts.Target == nil || opts.Target.ServiceMesh == nil {
		return nil
	}
	keys := make([]string, 0, len(opts.Target.ServiceMesh.Services))
	for key := range opts.Target.ServiceMesh.Services {
		if module, _, _ := strings.Cut(key, "/"); module == opts.Module {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		_, service, _ := strings.Cut(key, "/")
		unitDir := ""
		for idx := range opts.Units {
			unit := &opts.Units[idx]
			if unit.Kind == UnitKindService && unit.Name == service && !unit.Managed && unit.Path != "" {
				unitDir = filepath.Join(owned, filepath.FromSlash(unit.Path))
			}
		}
		if unitDir == "" {
			return fmt.Errorf("service-mesh %s: module %s renders no service %s to label", key, opts.Module, service)
		}
		overlay, err := renderOverlay(tree, unitDir, opts.Environment)
		if err != nil {
			return err
		}
		manifests, err := overlayManifestsInTree(tree, overlay)
		if err != nil {
			return err
		}
		labels := opts.Target.ServiceMeshLabels(opts.Module, service)
		var patches []serviceLabels
		for _, item := range manifests {
			if item.group != "" || item.kind != kindService {
				continue
			}
			name := metadataString(item.value, keyName)
			rendered := mapField(mapField(item.value, keyMetadata), "labels")
			for label := range labels {
				if _, owns := rendered[label]; owns {
					return fmt.Errorf("service-mesh %s: Service %s already carries label %q, which the render owns", key, name, label)
				}
			}
			patches = append(patches, serviceLabels{name: name, labels: labels})
		}
		if len(patches) == 0 {
			return fmt.Errorf("service-mesh %s: the service renders no Service to label", key)
		}
		if err = addOverlayPatches(tree, unitDir, opts.Environment, patches); err != nil {
			return fmt.Errorf("service-mesh %s: %w", key, err)
		}
	}
	return nil
}

// serviceLabels adds labels to one Service's metadata.
type serviceLabels struct {
	name   string
	labels map[string]string
}

func (projection serviceLabels) target() (kind, name string) { return kindService, projection.name }

func (projection serviceLabels) patch() (map[string]any, error) {
	encoded, err := yaml.Marshal(map[string]any{
		keyAPIVersion: "v1",
		keyKind:       kindService,
		keyMetadata:   map[string]any{keyName: projection.name, "labels": projection.labels},
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{keyPatchTarget: map[string]any{"version": "v1", keyKind: kindService, keyName: projection.name}, "patch": string(encoded)}, nil
}

func (projection serviceLabels) applyTo(document map[string]any) bool {
	if kind, _ := document[keyKind].(string); kind != kindService || metadataString(document, keyName) != projection.name {
		return false
	}
	metadata := mapField(document, keyMetadata)
	if metadata == nil {
		return false
	}
	labels := mapField(metadata, "labels")
	if labels == nil {
		labels = map[string]any{}
		metadata["labels"] = labels
	}
	for key, value := range projection.labels {
		labels[key] = value
	}
	return true
}
