package gitops

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

const gatewayAPIGroup = "gateway.networking.k8s.io"

// CloudProfile is codefly's cloud-aware translation for one cluster kind. The
// storage-neutral base carries no storageClassName and routing is owned by the
// mesh envelope, so the per-cloud slice reduces to the StorageClass the cloud's
// CSI driver provisions plus the load-balancer annotations its gateway needs.
type CloudProfile struct {
	Kind               string
	StorageClass       string
	GatewayAnnotations map[string]string
}

// cloudProfiles is keyed by EnvironmentCluster.Kind. Only managed clouds appear:
// local kinds (k3d, kind, minikube) run on the storage-neutral base with their
// cluster-default StorageClass and get no cloud component.
var cloudProfiles = map[string]CloudProfile{
	"eks": {
		Kind:         "eks",
		StorageClass: "gp3",
		GatewayAnnotations: map[string]string{
			"service.beta.kubernetes.io/aws-load-balancer-type": "external",
		},
	},
	"gke": {
		Kind:         "gke",
		StorageClass: "premium-rwo",
		GatewayAnnotations: map[string]string{
			"networking.gke.io/load-balancer-type": "External",
		},
	},
	"aks": {
		Kind:         "aks",
		StorageClass: "managed-csi",
		GatewayAnnotations: map[string]string{
			"service.beta.kubernetes.io/azure-load-balancer-internal": "false",
		},
	},
}

// CloudProfileForKind returns the cloud profile for a cluster kind. The second
// result is false for cluster kinds that need no cloud component.
func CloudProfileForKind(kind string) (CloudProfile, bool) {
	profile, ok := cloudProfiles[kind]
	return profile, ok
}

// RenderCloudComponent writes the per-cloud kustomize component for the
// environment's cluster kind under root and returns its root-relative slash
// path. It returns an empty path when the cluster kind has no cloud slice.
func RenderCloudComponent(root string, env *resources.Environment) (string, error) {
	if env.Cluster == nil || env.Cluster.Kind == "" {
		return "", fmt.Errorf("environment %q requires an explicit cluster kind to render a cloud component", env.Name)
	}
	profile, ok := CloudProfileForKind(env.Cluster.Kind)
	if !ok {
		return "", nil
	}
	return writeCloudComponent(root, profile)
}

func writeCloudComponent(root string, profile CloudProfile) (string, error) {
	relative := filepath.Join("components", "cloud", profile.Kind)
	directory := filepath.Join(root, relative)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", fmt.Errorf("create cloud component directory: %w", err)
	}
	patches := []map[string]any{
		{
			"target": map[string]any{"kind": "PersistentVolumeClaim"},
			"patch":  storageClassPatch(profile.StorageClass),
		},
	}
	if len(profile.GatewayAnnotations) > 0 {
		patches = append(patches, map[string]any{
			"target": map[string]any{"group": gatewayAPIGroup, "kind": "Gateway"},
			"patch":  gatewayAnnotationsPatch(profile.GatewayAnnotations),
		})
	}
	component := map[string]any{
		"apiVersion": "kustomize.config.k8s.io/v1alpha1",
		"kind":       "Component",
		"patches":    patches,
	}
	data, err := yaml.Marshal(component)
	if err != nil {
		return "", fmt.Errorf("encode cloud component: %w", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "kustomization.yaml"), data, 0o644); err != nil { //nolint:gosec
		return "", fmt.Errorf("write cloud component: %w", err)
	}
	return filepath.ToSlash(relative), nil
}

// The target selector matches every resource of the kind, so metadata.name in
// these strategic-merge bodies is required by the parser but never used to match.
const cloudPatchAnchorName = "cloud-profile"

func storageClassPatch(class string) string {
	patch := map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata":   map[string]any{"name": cloudPatchAnchorName},
		"spec":       map[string]any{"storageClassName": class},
	}
	return marshalPatch(patch)
}

func gatewayAnnotationsPatch(annotations map[string]string) string {
	patch := map[string]any{
		"apiVersion": gatewayAPIGroup + "/v1",
		"kind":       "Gateway",
		"metadata": map[string]any{
			"name":        cloudPatchAnchorName,
			"annotations": annotations,
		},
	}
	return marshalPatch(patch)
}

func marshalPatch(patch map[string]any) string {
	data, err := yaml.Marshal(patch)
	if err != nil {
		// The inputs are static maps of strings; marshalling cannot fail.
		panic(fmt.Sprintf("encode cloud patch: %v", err))
	}
	return string(data)
}
