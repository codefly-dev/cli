package gitops

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// --- A configuration change rolls the pods that read it ---
//
// A pod reads its ConfigMaps and Secrets once, at start: an envFrom or an
// env valueFrom is resolved when the container is created and never again.
// A render that changes only a ConfigMap's data therefore delivers the new
// bytes into the cluster and leaves every pod running on the old ones, for
// as long as nothing else restarts them — while a local run restarts with the
// new configuration every time. That is a local≠deployed difference, and the
// render closes it: every pod template of a serving workload is stamped with
// ConfigurationDigestAnnotation, a digest of everything it reads its
// configuration from, so a change to any of it is a change to the pod
// template, and the controller rolls the pods.
//
// What the digest covers, per referenced object, as the unit's overlay builds:
//
//   - a ConfigMap: its data and binaryData;
//   - a Secret materialized by an ExternalSecret: that ExternalSecret's spec —
//     which remote keys, properties and versions it reads and the template it
//     assembles them with — never a value, which the render never holds;
//   - a Secret the overlay renders itself (never on a promotable tree, and
//     never with values on any tree — validateManifest), or an object the unit
//     does not render (a platform ConfigMap, another unit's Secret): its name
//     alone, since its content is not this render's to read.
//
// Deployments, StatefulSets and DaemonSets are stamped. A Job's pod template
// is immutable once created, so a value that moves with the configuration
// would fail the apply of an unchanged Job; a Job and a CronJob's next Job
// read the configuration current when they start, so neither needs one.
//
// The digest is a function of the built manifests alone — canonical JSON,
// sorted — so identical inputs render byte-identical trees.

// ConfigurationDigestAnnotation is the pod-template annotation carrying the
// digest of the configuration a workload's pods read. The render owns it and
// refuses a workload that already declares it.
const ConfigurationDigestAnnotation = "codefly.dev/configuration-digest"

// keyTemplate is a workload spec's pod template field.
const keyTemplate = "template"

// projectConfigurationDigest stamps every serving workload of every unit's
// environment overlay with the digest of the configuration it reads.
func projectConfigurationDigest(tree *renderRoot, owned string, opts *RenderOptions) error {
	if opts.Environment == "" {
		return nil
	}
	for index := range opts.Units {
		unit := &opts.Units[index]
		if unit.Path == "" {
			continue
		}
		unitDir := filepath.Join(owned, filepath.FromSlash(unit.Path))
		if _, err := tree.stat(unitDir, filepath.Join("overlays", opts.Environment)); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		overlay, err := renderOverlay(tree, unitDir, opts.Environment)
		if err != nil {
			return err
		}
		manifests, err := overlayManifestsInTree(tree, overlay)
		if err != nil {
			return err
		}
		stamps, err := configurationDigests(manifests)
		if err != nil {
			return fmt.Errorf("unit %s: %w", unit.Name, err)
		}
		if err = addOverlayPatches(tree, unitDir, opts.Environment, stamps); err != nil {
			return fmt.Errorf("unit %s: %w", unit.Name, err)
		}
	}
	return nil
}

// configurationSource is one object a pod reads configuration from, as it
// enters the digest.
type configurationSource struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Content any    `json:"content,omitempty"`
}

// configurationDigests is the stamp of each serving workload among the built
// manifests that reads any configuration; a workload that reads none is left
// as it is.
func configurationDigests(manifests []manifest) ([]podTemplateAnnotation, error) {
	configMaps := map[string]map[string]any{}
	externalSecrets := map[string]any{}
	for _, item := range manifests {
		namespace := metadataString(item.value, keyNamespace)
		switch {
		case item.group == "" && item.kind == kindConfigMap:
			configMaps[namespace+"/"+metadataString(item.value, keyName)] = map[string]any{
				dataKey: item.value[dataKey], "binaryData": item.value["binaryData"],
			}
		case item.group == externalSecretsGroup && item.kind == kindExternalSecret:
			spec := mapField(item.value, keySpec)
			target, _ := mapField(spec, "target")[keyName].(string)
			if target == "" {
				target = metadataString(item.value, keyName)
			}
			externalSecrets[namespace+"/"+target] = spec
		}
	}
	var stamps []podTemplateAnnotation
	for _, item := range manifests {
		if item.group != "apps" || !servingKind(item.kind) {
			continue
		}
		spec, ok := podSpec(item)
		if !ok {
			continue
		}
		name := metadataString(item.value, keyName)
		if _, owned := mapField(mapField(podTemplate(item), keyMetadata), "annotations")[ConfigurationDigestAnnotation]; owned {
			return nil, fmt.Errorf("%s %s carries the pod-template annotation %s, which the render owns; remove it from the manifests", item.kind, name, ConfigurationDigestAnnotation)
		}
		configMapNames, secretNames := podConfigurationReferences(spec)
		if len(configMapNames) == 0 && len(secretNames) == 0 {
			continue
		}
		namespace := metadataString(item.value, keyNamespace)
		sources := make([]configurationSource, 0, len(configMapNames)+len(secretNames))
		for _, referenced := range configMapNames {
			source := configurationSource{Kind: kindConfigMap, Name: referenced}
			if content, rendered := configMaps[namespace+"/"+referenced]; rendered {
				source.Content = content
			}
			sources = append(sources, source)
		}
		for _, referenced := range secretNames {
			source := configurationSource{Kind: "Secret", Name: referenced}
			if spec, materialized := externalSecrets[namespace+"/"+referenced]; materialized {
				source.Kind, source.Content = kindExternalSecret, spec
			}
			sources = append(sources, source)
		}
		digest, err := configurationDigest(sources)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", item.kind, name, err)
		}
		stamps = append(stamps, podTemplateAnnotation{kind: item.kind, name: name, key: ConfigurationDigestAnnotation, value: digest})
	}
	return stamps, nil
}

// configurationDigest is "sha256:" and the hex SHA-256 of the sources'
// canonical JSON (maps encode with sorted keys; the sources arrive sorted).
func configurationDigest(sources []configurationSource) (string, error) {
	encoded, err := json.Marshal(sources)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// podConfigurationReferences names, sorted and once each, the ConfigMaps and
// Secrets a pod spec reads: through any container's or init container's
// envFrom and env valueFrom, and through its volumes, projected ones included.
func podConfigurationReferences(spec map[string]any) (configMaps, secrets []string) {
	configMapSet, secretSet := map[string]struct{}{}, map[string]struct{}{}
	add := func(set map[string]struct{}, reference map[string]any, key string) {
		if name, _ := reference[key].(string); name != "" {
			set[name] = struct{}{}
		}
	}
	for _, field := range []string{keyContainers, "initContainers"} {
		for _, raw := range sliceField(spec, field) {
			container, _ := raw.(map[string]any)
			for _, rawSource := range sliceField(container, "envFrom") {
				source, _ := rawSource.(map[string]any)
				add(configMapSet, mapField(source, "configMapRef"), keyName)
				add(secretSet, mapField(source, "secretRef"), keyName)
			}
			for _, rawVariable := range sliceField(container, "env") {
				variable, _ := rawVariable.(map[string]any)
				from := mapField(variable, "valueFrom")
				add(configMapSet, mapField(from, "configMapKeyRef"), keyName)
				add(secretSet, mapField(from, "secretKeyRef"), keyName)
			}
		}
	}
	for _, raw := range sliceField(spec, "volumes") {
		volume, _ := raw.(map[string]any)
		add(configMapSet, mapField(volume, keyConfigMap), keyName)
		add(secretSet, mapField(volume, "secret"), "secretName")
		for _, rawSource := range sliceField(mapField(volume, "projected"), "sources") {
			source, _ := rawSource.(map[string]any)
			add(configMapSet, mapField(source, keyConfigMap), keyName)
			add(secretSet, mapField(source, "secret"), keyName)
		}
	}
	return sortedNames(configMapSet), sortedNames(secretSet)
}

func sortedNames(set map[string]struct{}) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// podTemplateAnnotation sets one annotation on a workload's pod template.
type podTemplateAnnotation struct {
	kind, name string
	key, value string
}

func (stamp podTemplateAnnotation) target() (kind, name string) { return stamp.kind, stamp.name }

func (stamp podTemplateAnnotation) patch() (map[string]any, error) {
	encoded, err := yaml.Marshal(map[string]any{
		keyAPIVersion: "apps/v1",
		keyKind:       stamp.kind,
		keyMetadata:   map[string]any{keyName: stamp.name},
		keySpec: map[string]any{keyTemplate: map[string]any{
			keyMetadata: map[string]any{"annotations": map[string]any{stamp.key: stamp.value}},
		}},
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{keyPatchTarget: map[string]any{keyKind: stamp.kind, keyName: stamp.name}, keyPatch: string(encoded)}, nil
}

func (stamp podTemplateAnnotation) applyTo(document map[string]any) bool {
	if kind, _ := document[keyKind].(string); kind != stamp.kind || metadataString(document, keyName) != stamp.name {
		return false
	}
	template := mapField(mapField(document, keySpec), keyTemplate)
	if template == nil {
		return false
	}
	metadata := mapField(template, keyMetadata)
	if metadata == nil {
		metadata = map[string]any{}
		template[keyMetadata] = metadata
	}
	annotations := mapField(metadata, "annotations")
	if annotations == nil {
		annotations = map[string]any{}
		metadata["annotations"] = annotations
	}
	annotations[stamp.key] = stamp.value
	return true
}
