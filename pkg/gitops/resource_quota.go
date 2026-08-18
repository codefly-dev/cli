package gitops

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

const (
	resourceQuotaName    = "codefly-resource-quota"
	limitRangeName       = "codefly-container-defaults"
	resourceQuotaFile    = "resource-quota.yaml"
	limitRangeFile       = "limit-range.yaml"
	containerLimitType   = "Container"
	resourceQuotaKind    = "ResourceQuota"
	limitRangeKind       = "LimitRange"
	resourceQuotaVersion = "v1"
)

// namespacedMeta is the metadata shared by the namespace-scoped resources render
// synthesizes from consumer-owned config.
type namespacedMeta struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

type resourceQuota struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   namespacedMeta    `yaml:"metadata"`
	Spec       resourceQuotaSpec `yaml:"spec"`
}

type resourceQuotaSpec struct {
	Hard map[string]string `yaml:"hard"`
}

type limitRange struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   namespacedMeta `yaml:"metadata"`
	Spec       limitRangeSpec `yaml:"spec"`
}

type limitRangeSpec struct {
	Limits []limitRangeItem `yaml:"limits"`
}

type limitRangeItem struct {
	Type           string            `yaml:"type"`
	Default        map[string]string `yaml:"default,omitempty"`
	DefaultRequest map[string]string `yaml:"defaultRequest,omitempty"`
}

// resourceQuotaManifest renders the ResourceQuota that caps a namespace's total
// compute from an environment's declared resource-quota. It returns nil when the
// declaration sets no hard cap (only a LimitRange of container defaults), so the
// caller emits a LimitRange without an empty, ownership-claiming ResourceQuota.
func resourceQuotaManifest(namespace string, quota *resources.EnvironmentResourceQuota) *resourceQuota {
	hard := map[string]string{}
	if quota.Requests != nil {
		putQuantity(hard, "requests.cpu", quota.Requests.CPU)
		putQuantity(hard, "requests.memory", quota.Requests.Memory)
	}
	if quota.Limits != nil {
		putQuantity(hard, "limits.cpu", quota.Limits.CPU)
		putQuantity(hard, "limits.memory", quota.Limits.Memory)
	}
	putQuantity(hard, "pods", quota.Pods)
	if len(hard) == 0 {
		return nil
	}
	return &resourceQuota{
		APIVersion: resourceQuotaVersion,
		Kind:       resourceQuotaKind,
		Metadata:   namespacedMeta{Name: resourceQuotaName, Namespace: namespace},
		Spec:       resourceQuotaSpec{Hard: hard},
	}
}

// limitRangeManifest renders the LimitRange that gives every container in a
// namespace default requests/limits, so a pod that omits them still receives
// values and counts against the ResourceQuota instead of being rejected. It
// returns nil when the declaration carries no container defaults.
func limitRangeManifest(namespace string, quota *resources.EnvironmentResourceQuota) *limitRange {
	defaults := quota.DefaultContainer
	if defaults == nil {
		return nil
	}
	item := limitRangeItem{Type: containerLimitType}
	if defaults.Limits != nil {
		item.Default = resourceMap(defaults.Limits)
	}
	if defaults.Requests != nil {
		item.DefaultRequest = resourceMap(defaults.Requests)
	}
	if len(item.Default) == 0 && len(item.DefaultRequest) == 0 {
		return nil
	}
	return &limitRange{
		APIVersion: resourceQuotaVersion,
		Kind:       limitRangeKind,
		Metadata:   namespacedMeta{Name: limitRangeName, Namespace: namespace},
		Spec:       limitRangeSpec{Limits: []limitRangeItem{item}},
	}
}

func resourceMap(list *resources.EnvironmentResourceList) map[string]string {
	values := map[string]string{}
	putQuantity(values, "cpu", list.CPU)
	putQuantity(values, "memory", list.Memory)
	if len(values) == 0 {
		return nil
	}
	return values
}

func putQuantity(target map[string]string, key, value string) {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		target[key] = trimmed
	}
}

// projectResourceQuota writes the namespace's ResourceQuota (and optional
// container-defaults LimitRange) into a bootstrap overlay and adds them to that
// overlay's kustomization, so the caps land in the same promoted Application
// that declares the namespace. It is a no-op — reporting false — when the
// environment declares no resource-quota. Because the caps are namespace-scoped
// they must resolve to a namespace, so an environment without one is an error.
func projectResourceQuota(bootstrapRoot, environment, namespace string, quota *resources.EnvironmentResourceQuota) (bool, error) {
	if quota == nil {
		return false, nil
	}
	if namespace == "" {
		return false, fmt.Errorf("environment %q declares a resource-quota but has no namespace", environment)
	}
	overlay := filepath.Join(bootstrapRoot, "overlays", environment)
	if info, err := os.Stat(overlay); err != nil || !info.IsDir() {
		return false, fmt.Errorf("environment %q declares a resource-quota but its bootstrap has no %q overlay to render it into", environment, environment)
	}
	projected := false
	if manifest := resourceQuotaManifest(namespace, quota); manifest != nil {
		if err := writeOverlayResource(overlay, resourceQuotaFile, manifest); err != nil {
			return false, err
		}
		projected = true
	}
	if manifest := limitRangeManifest(namespace, quota); manifest != nil {
		if err := writeOverlayResource(overlay, limitRangeFile, manifest); err != nil {
			return false, err
		}
		projected = true
	}
	return projected, nil
}

func writeOverlayResource(overlay, name string, manifest any) error {
	encoded, err := yaml.Marshal(manifest)
	if err != nil {
		return err
	}
	// A quota/limit manifest is a plain resource cap, world-readable like its siblings.
	if err := os.WriteFile(filepath.Join(overlay, name), encoded, 0o644); err != nil { //nolint:gosec
		return err
	}
	return addKustomizationResource(overlay, name)
}
