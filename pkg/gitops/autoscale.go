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
	hpaAPIVersion    = "autoscaling/v2"
	hpaKind          = "HorizontalPodAutoscaler"
	hpaFile          = "hpa.yaml"
	deploymentKind   = "Deployment"
	deploymentGroup  = "apps"
	deploymentAPIVer = "apps/v1"
)

type horizontalPodAutoscaler struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   namespacedMeta `yaml:"metadata"`
	Spec       hpaSpec        `yaml:"spec"`
}

type hpaSpec struct {
	ScaleTargetRef hpaScaleTargetRef `yaml:"scaleTargetRef"`
	MinReplicas    int32             `yaml:"minReplicas"`
	MaxReplicas    int32             `yaml:"maxReplicas"`
	Metrics        []hpaMetric       `yaml:"metrics"`
}

type hpaScaleTargetRef struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Name       string `yaml:"name"`
}

type hpaMetric struct {
	Type     string            `yaml:"type"`
	Resource hpaResourceMetric `yaml:"resource"`
}

type hpaResourceMetric struct {
	Name   string          `yaml:"name"`
	Target hpaMetricTarget `yaml:"target"`
}

type hpaMetricTarget struct {
	Type               string `yaml:"type"`
	AverageUtilization int32  `yaml:"averageUtilization"`
}

// autoscaleManifest renders the HorizontalPodAutoscaler that scales a service's
// Deployment from its declared autoscale block. The scaleTargetRef names the
// Deployment as authored in the rendered tree.
//
// This relies on the same invariant the secret projection relies on for
// secret-<service>: the promotable overlay does not rename the workload. When
// the overlay itself renames the Deployment, kustomize's built-in name
// reference rewrites this scaleTargetRef in the same build. But a name change
// applied in the base — before the overlay composes the HPA in — would leave
// this ref pointing at the pre-rename name with no error, only a silently
// idle HPA. Promotable overlays pin images by digest rather than renaming
// workloads, so that case does not arise; if that ever changes, the HPA (and
// every fixed-name reference like secret-<service>) breaks together.
func autoscaleManifest(service, namespace, deployment string, autoscale *resources.ServiceAutoscale) *horizontalPodAutoscaler {
	return &horizontalPodAutoscaler{
		APIVersion: hpaAPIVersion,
		Kind:       hpaKind,
		Metadata:   namespacedMeta{Name: service, Namespace: namespace},
		Spec: hpaSpec{
			ScaleTargetRef: hpaScaleTargetRef{APIVersion: deploymentAPIVer, Kind: deploymentKind, Name: deployment},
			MinReplicas:    autoscale.Min,
			MaxReplicas:    autoscale.Max,
			Metrics: []hpaMetric{{
				Type: "Resource",
				Resource: hpaResourceMetric{
					Name:   "cpu",
					Target: hpaMetricTarget{Type: "Utilization", AverageUtilization: autoscale.TargetCPU},
				},
			}},
		},
	}
}

// projectServiceAutoscale writes a service's HorizontalPodAutoscaler into its
// environment overlay and adds it to that overlay's kustomization, mirroring the
// secret projection so an env-scoped promotion carries it. It is a no-op —
// reporting false — when the service declares no autoscale block. The
// scaleTargetRef points at the single Deployment the service's rendered tree
// contains; a tree with zero or several Deployments is ambiguous and fails.
func projectServiceAutoscale(serviceRoot, service, environment, namespace string, autoscale *resources.ServiceAutoscale) (bool, error) {
	if autoscale == nil {
		return false, nil
	}
	if namespace == "" {
		return false, fmt.Errorf("service %q declares autoscale but its environment has no namespace", service)
	}
	deployment, err := serviceDeploymentName(serviceRoot, service)
	if err != nil {
		return false, err
	}
	overlay := filepath.Join(serviceRoot, "overlays", environment)
	if info, statErr := os.Stat(overlay); statErr != nil || !info.IsDir() {
		return false, fmt.Errorf("service %q declares autoscale but has no %q environment overlay to render it into", service, environment)
	}
	encoded, err := yaml.Marshal(autoscaleManifest(service, namespace, deployment, autoscale))
	if err != nil {
		return false, err
	}
	// The HPA is a plain scaling policy, world-readable like its siblings.
	if err := os.WriteFile(filepath.Join(overlay, hpaFile), encoded, 0o644); err != nil { //nolint:gosec
		return false, err
	}
	return true, addKustomizationResource(overlay, hpaFile)
}

// serviceDeploymentName returns the metadata.name of the sole Deployment in a
// service's rendered tree, which the HPA scales.
func serviceDeploymentName(root, service string) (string, error) {
	var names []string
	err := walkRegularFiles(root, func(path, relative string, _ os.FileInfo) error {
		extension := strings.ToLower(filepath.Ext(relative))
		if extension != yamlExtension && extension != ymlExtension && extension != jsonExtension {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		manifests, _, err := decodeYAML(relative, data)
		if err != nil {
			return err
		}
		for _, item := range manifests {
			if item.group == deploymentGroup && item.kind == deploymentKind {
				if name := metadataString(item.value, "name"); name != "" {
					names = append(names, name)
				}
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	unique := map[string]struct{}{}
	for _, name := range names {
		unique[name] = struct{}{}
	}
	if len(unique) != 1 {
		return "", fmt.Errorf("service %q declares autoscale but its rendered tree contains %d Deployments, need exactly one", service, len(unique))
	}
	return names[0], nil
}
