package gitops

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// CloudProfile is codefly's cloud-aware translation for one cluster kind. The
// storage-neutral base carries no storageClassName, so the per-cloud slice
// injects the StorageClass the cloud's CSI driver provisions.
//
// Storage must be modelled as a standalone PersistentVolumeClaim (the codefly
// convention: a StatefulSet or Deployment mounts a separate PVC resource). The
// component patches kind: PersistentVolumeClaim, so a StatefulSet that instead
// declares inline spec.volumeClaimTemplates would not receive the class — that
// shape is outside codefly's storage model and is not supported here.
type CloudProfile struct {
	Kind         string
	StorageClass string
}

// cloudProfiles is keyed by EnvironmentCluster.Kind and holds the managed clouds
// whose per-cloud slice differs from the storage-neutral base.
var cloudProfiles = map[string]CloudProfile{
	"eks": {Kind: "eks", StorageClass: "gp3"},
	"gke": {Kind: "gke", StorageClass: "premium-rwo"},
	"aks": {Kind: "aks", StorageClass: "managed-csi"},
}

// localClusterKinds are the cluster kinds that legitimately need no cloud
// component: they run on the storage-neutral base with the cluster-default
// StorageClass (local clusters) or manage storage outside codefly (external).
var localClusterKinds = map[string]struct{}{
	"k3d":      {},
	"kind":     {},
	"minikube": {},
	"external": {},
}

// CloudProfileForKind returns the cloud profile for a cluster kind. The second
// result is false for cluster kinds that need no cloud component.
func CloudProfileForKind(kind string) (CloudProfile, bool) {
	profile, ok := cloudProfiles[kind]
	return profile, ok
}

// RenderCloudComponent writes the per-cloud kustomize component for the
// environment's cluster kind under root and returns its root-relative slash
// path. It returns an empty path for cluster kinds that need no cloud component,
// and an error for an unrecognized cluster kind so a typo cannot silently ship
// storage-neutral manifests to a managed cloud.
func RenderCloudComponent(root string, env *resources.Environment) (string, error) {
	if env.Cluster == nil || env.Cluster.Kind == "" {
		return "", fmt.Errorf("environment %q requires an explicit cluster kind to render a cloud component", env.Name)
	}
	kind := env.Cluster.Kind
	profile, ok := CloudProfileForKind(kind)
	if !ok {
		if _, local := localClusterKinds[kind]; local {
			return "", nil
		}
		return "", fmt.Errorf("environment %q has unrecognized cluster kind %q", env.Name, kind)
	}
	return writeCloudComponent(root, profile)
}

func writeCloudComponent(root string, profile CloudProfile) (string, error) {
	relative := filepath.Join("components", "cloud", profile.Kind)
	directory := filepath.Join(root, relative)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", fmt.Errorf("create cloud component directory: %w", err)
	}
	component := map[string]any{
		"apiVersion": "kustomize.config.k8s.io/v1alpha1",
		"kind":       "Component",
		"patches": []map[string]any{
			{
				"target": map[string]any{"kind": "PersistentVolumeClaim"},
				"patch":  storageClassPatch(profile.StorageClass),
			},
		},
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

// The target selector matches every PersistentVolumeClaim, so metadata.name in
// the strategic-merge body is required by the parser but never used to match.
const cloudPatchAnchorName = "cloud-profile"

func storageClassPatch(class string) string {
	patch := map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata":   map[string]any{"name": cloudPatchAnchorName},
		"spec":       map[string]any{"storageClassName": class},
	}
	data, err := yaml.Marshal(patch)
	if err != nil {
		// The input is a static map of strings; marshalling cannot fail.
		panic(fmt.Sprintf("encode cloud patch: %v", err))
	}
	return string(data)
}
