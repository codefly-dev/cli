package gitops

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

func TestCloudProfileForKind(t *testing.T) {
	cases := map[string]struct {
		found        bool
		storageClass string
	}{
		"eks":      {found: true, storageClass: "gp3"},
		"gke":      {found: true, storageClass: "premium-rwo"},
		"aks":      {found: true, storageClass: "managed-csi"},
		"k3d":      {found: false},
		"kind":     {found: false},
		"external": {found: false},
		"":         {found: false},
		"nonsense": {found: false},
	}
	for kind, expected := range cases {
		profile, ok := CloudProfileForKind(kind)
		require.Equal(t, expected.found, ok, "kind %q", kind)
		if expected.found {
			require.Equal(t, kind, profile.Kind)
			require.Equal(t, expected.storageClass, profile.StorageClass)
		}
	}
}

func TestRenderCloudComponentSkipsLocalClusters(t *testing.T) {
	for _, kind := range []string{"k3d", "kind", "minikube", "external"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			relative, err := RenderCloudComponent(root, &resources.Environment{
				Name:    "local",
				Cluster: &resources.EnvironmentCluster{Kind: kind},
			})
			require.NoError(t, err)
			require.Empty(t, relative)
			_, statErr := os.Stat(filepath.Join(root, "components"))
			require.True(t, os.IsNotExist(statErr))
		})
	}
}

func TestRenderCloudComponentRejectsUnknownKind(t *testing.T) {
	// A typo or unlisted kind must error, not silently emit no component and
	// ship storage-neutral manifests to a managed cloud.
	for _, kind := range []string{"EKS", "aws", "gcp", "azure"} {
		root := t.TempDir()
		_, err := RenderCloudComponent(root, &resources.Environment{
			Name:    "cloud",
			Cluster: &resources.EnvironmentCluster{Kind: kind},
		})
		require.Error(t, err, "kind %q", kind)
	}
}

func TestRenderCloudComponentRequiresClusterKind(t *testing.T) {
	root := t.TempDir()
	_, err := RenderCloudComponent(root, &resources.Environment{Name: "aws"})
	require.Error(t, err)
	_, err = RenderCloudComponent(root, &resources.Environment{
		Name:    "aws",
		Cluster: &resources.EnvironmentCluster{},
	})
	require.Error(t, err)
}

func TestRenderCloudComponentAppliesStorageClass(t *testing.T) {
	for _, kind := range []string{"eks", "gke", "aks"} {
		t.Run(kind, func(t *testing.T) {
			profile, _ := CloudProfileForKind(kind)
			root := writeCloudFixture(t)

			relative, err := RenderCloudComponent(root, &resources.Environment{
				Name:    "cloud",
				Cluster: &resources.EnvironmentCluster{Kind: kind},
			})
			require.NoError(t, err)
			require.Equal(t, filepath.ToSlash(filepath.Join("components", "cloud", kind)), relative)

			manifests := buildOverlayWithComponent(t, root, relative)
			pvc := manifestByKind(t, manifests, pvcKind)
			spec, _ := pvc["spec"].(map[string]any)
			require.Equal(t, profile.StorageClass, spec["storageClassName"])
		})
	}
}

// The codefly stateful pattern is a StatefulSet that mounts a standalone PVC
// resource (not inline volumeClaimTemplates); confirm that PVC is patched.
func TestRenderCloudComponentPatchesPVCMountedByStatefulSet(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "base", "kustomization.yaml"), map[string]any{
		"apiVersion": "kustomize.config.k8s.io/v1beta1",
		"kind":       "Kustomization",
		"resources":  []string{"sts.yaml", "pvc.yaml"},
	})
	writeFile(t, filepath.Join(root, "base", "pvc.yaml"), map[string]any{
		"apiVersion": "v1",
		"kind":       pvcKind,
		"metadata":   map[string]any{"name": "data"},
		"spec": map[string]any{
			"accessModes": []string{"ReadWriteOnce"},
			"resources":   map[string]any{"requests": map[string]any{"storage": "1Gi"}},
		},
	})
	writeFile(t, filepath.Join(root, "base", "sts.yaml"), map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "StatefulSet",
		"metadata":   map[string]any{"name": "db"},
		"spec": map[string]any{
			"serviceName": "db",
			"selector":    map[string]any{"matchLabels": map[string]any{"app": "db"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "db"}},
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "db", "image": "postgres"}},
					"volumes": []any{map[string]any{
						"name":                  "data",
						"persistentVolumeClaim": map[string]any{"claimName": "data"},
					}},
				},
			},
		},
	})

	relative, err := RenderCloudComponent(root, &resources.Environment{
		Name:    "cloud",
		Cluster: &resources.EnvironmentCluster{Kind: "eks"},
	})
	require.NoError(t, err)

	manifests := buildOverlayWithComponent(t, root, relative)
	pvc := manifestByKind(t, manifests, pvcKind)
	spec, _ := pvc["spec"].(map[string]any)
	require.Equal(t, "gp3", spec["storageClassName"])
}

func TestRenderCloudComponentLeavesBaseStorageNeutral(t *testing.T) {
	root := writeCloudFixture(t)
	manifests := buildKustomize(t, filepath.Join(root, "base"))
	pvc := manifestByKind(t, manifests, pvcKind)
	spec, _ := pvc["spec"].(map[string]any)
	_, hasStorageClass := spec["storageClassName"]
	require.False(t, hasStorageClass)
}

func writeCloudFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "base", "kustomization.yaml"), map[string]any{
		"apiVersion": "kustomize.config.k8s.io/v1beta1",
		"kind":       "Kustomization",
		"resources":  []string{"pvc.yaml"},
	})
	writeFile(t, filepath.Join(root, "base", "pvc.yaml"), map[string]any{
		"apiVersion": "v1",
		"kind":       pvcKind,
		"metadata":   map[string]any{"name": "data"},
		"spec": map[string]any{
			"accessModes": []string{"ReadWriteOnce"},
			"resources":   map[string]any{"requests": map[string]any{"storage": "1Gi"}},
		},
	})
	return root
}

func buildOverlayWithComponent(t *testing.T, root, componentRelative string) []map[string]any {
	t.Helper()
	writeFile(t, filepath.Join(root, "overlay", "kustomization.yaml"), map[string]any{
		"apiVersion": "kustomize.config.k8s.io/v1beta1",
		"kind":       "Kustomization",
		"resources":  []string{"../base"},
		"components": []string{
			filepath.ToSlash(filepath.Join("..", componentRelative)),
		},
	})
	return buildKustomize(t, filepath.Join(root, "overlay"))
}

func writeFile(t *testing.T, path string, value any) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	data, err := yaml.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
}

func buildKustomize(t *testing.T, directory string) []map[string]any {
	t.Helper()
	kustomizer := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	resourceMap, err := kustomizer.Run(filesys.MakeFsOnDisk(), directory)
	require.NoError(t, err)
	data, err := resourceMap.AsYaml()
	require.NoError(t, err)
	var manifests []map[string]any
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var value map[string]any
		if decodeErr := decoder.Decode(&value); decodeErr != nil {
			break
		}
		if value != nil {
			manifests = append(manifests, value)
		}
	}
	return manifests
}

func manifestByKind(t *testing.T, manifests []map[string]any, kind string) map[string]any {
	t.Helper()
	for _, manifest := range manifests {
		if manifest["kind"] == kind {
			return manifest
		}
	}
	t.Fatalf("no %s in rendered output", kind)
	return nil
}
