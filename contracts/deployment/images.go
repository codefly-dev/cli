package deployment

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

const ociManifest = "application/vnd.oci.image.manifest.v1+json"
const dockerManifest = "application/vnd.docker.distribution.manifest.v2+json"

func marshalCanonical(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return CanonicalJSON(b)
}
func validateImages(inv Inventory, profile ExecutionProfile, blobs map[string][]byte) error {
	for _, w := range inv.Workloads {
		for _, containers := range [][]Container{w.Template.Spec.Containers, w.Template.Spec.InitContainers} {
			for _, c := range containers {
				p := "$/workloads/" + w.ID + "/images/" + c.Name
				digest := c.Image[strings.LastIndex(c.Image, "@")+1:]
				raw, ok := blobs[digest]
				if !ok {
					return refuse("IMAGE_EVIDENCE", p, "retained manifest bytes missing")
				}
				value, err := parseJSON(raw)
				if err != nil {
					return err
				}
				m, ok := value.(map[string]any)
				if !ok {
					return refuse("IMAGE_MANIFEST", p, "manifest must be an object")
				}
				media, _ := m["mediaType"].(string)
				if media == "application/vnd.oci.image.index.v1+json" || media == "application/vnd.docker.distribution.manifest.list.v2+json" {
					return refuse("IMAGE_INDEX", p, "image indexes are not supported")
				}
				if (media != ociManifest && media != dockerManifest) || m["schemaVersion"] != json.Number("2") {
					return refuse("IMAGE_MANIFEST", p, "unsupported manifest media type/schema version")
				}
				desc, ok := m["config"].(map[string]any)
				if !ok {
					return refuse("IMAGE_CONFIG_LINK", p, "manifest configuration descriptor missing")
				}
				configDigest, _ := desc["digest"].(string)
				config, ok := blobs[configDigest]
				if !ok || !digestPattern.MatchString(configDigest) {
					return refuse("IMAGE_CONFIG_LINK", p, "retained configuration missing")
				}
				expectedMedia := "application/vnd.oci.image.config.v1+json"
				if media == dockerManifest {
					expectedMedia = "application/vnd.docker.container.image.v1+json"
				}
				if desc["mediaType"] != expectedMedia || desc["size"] != json.Number(fmt.Sprint(len(config))) {
					return refuse("IMAGE_CONFIG_LINK", p, "configuration media type or size mismatch")
				}
				configValue, err := parseJSON(config)
				if err != nil {
					return err
				}
				conf, ok := configValue.(map[string]any)
				if !ok {
					return refuse("IMAGE_CONFIGURATION", p, "image configuration must be an object")
				}
				platform := Platform{}
				platform.OS, _ = conf["os"].(string)
				platform.Architecture, _ = conf["architecture"].(string)
				if v, exists := conf["variant"]; exists {
					var valid bool
					platform.Variant, valid = v.(string)
					if !valid {
						return refuse("IMAGE_PLATFORM", p, "invalid platform variant")
					}
				}
				if !slices.Contains(profile.Platforms, platform) || platform.OS != w.Template.Spec.OS.Name || platform.Architecture != w.Template.Spec.NodeSelector["kubernetes.io/arch"] {
					return refuse("IMAGE_PLATFORM", p, "manifest platform is not enabled or does not match node selection")
				}
				layers, ok := m["layers"].([]any)
				if !ok {
					return refuse("IMAGE_MANIFEST", p, "layer descriptors must be explicit")
				}
				for _, layer := range layers {
					d, ok := layer.(map[string]any)
					if !ok {
						return refuse("IMAGE_MANIFEST", p, "invalid layer descriptor")
					}
					dg, _ := d["digest"].(string)
					sz, ok := d["size"].(json.Number)
					if !digestPattern.MatchString(dg) || !ok || strings.HasPrefix(string(sz), "-") {
						return refuse("IMAGE_MANIFEST", p, "invalid layer digest or size")
					}
				}
				root, ok := conf["rootfs"].(map[string]any)
				if !ok || root["type"] != "layers" {
					return refuse("IMAGE_CONFIGURATION", p, "rootfs layers required")
				}
				diffs, ok := root["diff_ids"].([]any)
				if !ok || len(diffs) != len(layers) {
					return refuse("IMAGE_CONFIGURATION", p, "configuration layer count mismatch")
				}
				for _, d := range diffs {
					dg, ok := d.(string)
					if !ok || !digestPattern.MatchString(dg) {
						return refuse("IMAGE_CONFIGURATION", p, "invalid rootfs digest")
					}
				}
			}
		}
	}
	return nil
}
