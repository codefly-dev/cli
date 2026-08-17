package gitops

import (
	"context"
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
// Deployment discovered in the rendered tree; kustomize's built-in name
// reference keeps it aligned if an overlay renames the Deployment.
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

// projectRenderedServiceAutoscale projects a HorizontalPodAutoscaler onto every
// service tree a single-service render produced — the origin service and any
// in-graph dependencies it pulled in — loading each service's autoscale
// declaration so per-service promotion renders the same HPAs as a full module
// render.
func projectRenderedServiceAutoscale(ctx context.Context, stage string, workspace *resources.Workspace, env *resources.Environment) error {
	modulesRoot := filepath.Join(stage, "modules")
	moduleEntries, err := os.ReadDir(modulesRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, moduleEntry := range moduleEntries {
		if !moduleEntry.IsDir() {
			continue
		}
		module, err := workspace.LoadModuleFromName(ctx, moduleEntry.Name())
		if err != nil {
			return fmt.Errorf("load module %s: %w", moduleEntry.Name(), err)
		}
		servicesRoot := filepath.Join(modulesRoot, moduleEntry.Name(), serviceUnitDir)
		serviceEntries, err := os.ReadDir(servicesRoot)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, serviceEntry := range serviceEntries {
			if !serviceEntry.IsDir() {
				continue
			}
			service, err := module.LoadServiceFromName(ctx, serviceEntry.Name())
			if err != nil {
				return fmt.Errorf("load service %s: %w", serviceEntry.Name(), err)
			}
			if _, err := projectServiceAutoscale(
				filepath.Join(servicesRoot, serviceEntry.Name()),
				serviceEntry.Name(),
				env.Name,
				env.Namespace,
				service.Autoscale,
			); err != nil {
				return fmt.Errorf("project service %s autoscale: %w", serviceEntry.Name(), err)
			}
		}
	}
	return nil
}
