package gitops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

func fullResourceQuota() *resources.EnvironmentResourceQuota {
	return &resources.EnvironmentResourceQuota{
		Requests: &resources.EnvironmentResourceList{CPU: "4", Memory: "8Gi"},
		Limits:   &resources.EnvironmentResourceList{CPU: "8", Memory: "16Gi"},
		Pods:     "50",
		DefaultContainer: &resources.EnvironmentContainerResources{
			Requests: &resources.EnvironmentResourceList{CPU: "100m", Memory: "128Mi"},
			Limits:   &resources.EnvironmentResourceList{CPU: "500m", Memory: "512Mi"},
		},
	}
}

func TestResourceQuotaManifestRendersHardCaps(t *testing.T) {
	manifest := resourceQuotaManifest("lodestar", fullResourceQuota())
	if manifest == nil {
		t.Fatal("full quota rendered no ResourceQuota")
	}
	if manifest.Kind != resourceQuotaKind || manifest.Metadata.Namespace != "lodestar" {
		t.Fatalf("manifest identity = %s in %s", manifest.Kind, manifest.Metadata.Namespace)
	}
	want := map[string]string{
		"requests.cpu": "4", "requests.memory": "8Gi",
		"limits.cpu": "8", "limits.memory": "16Gi", "pods": "50",
	}
	for key, value := range want {
		if manifest.Spec.Hard[key] != value {
			t.Fatalf("hard[%q] = %q, want %q", key, manifest.Spec.Hard[key], value)
		}
	}
	if len(manifest.Spec.Hard) != len(want) {
		t.Fatalf("hard map has %d entries, want %d: %v", len(manifest.Spec.Hard), len(want), manifest.Spec.Hard)
	}
}

func TestResourceQuotaManifestNilWithoutHardCaps(t *testing.T) {
	onlyDefaults := &resources.EnvironmentResourceQuota{
		DefaultContainer: &resources.EnvironmentContainerResources{
			Requests: &resources.EnvironmentResourceList{CPU: "100m"},
		},
	}
	if manifest := resourceQuotaManifest("lodestar", onlyDefaults); manifest != nil {
		t.Fatalf("quota with only container defaults rendered a ResourceQuota: %+v", manifest)
	}
}

func TestLimitRangeManifest(t *testing.T) {
	manifest := limitRangeManifest("lodestar", fullResourceQuota())
	if manifest == nil {
		t.Fatal("quota with container defaults rendered no LimitRange")
	}
	if len(manifest.Spec.Limits) != 1 || manifest.Spec.Limits[0].Type != containerLimitType {
		t.Fatalf("limit range items = %+v", manifest.Spec.Limits)
	}
	item := manifest.Spec.Limits[0]
	if item.Default["cpu"] != "500m" || item.DefaultRequest["memory"] != "128Mi" {
		t.Fatalf("limit range defaults = %+v", item)
	}
	if manifest := limitRangeManifest("lodestar", &resources.EnvironmentResourceQuota{Pods: "10"}); manifest != nil {
		t.Fatalf("quota without container defaults rendered a LimitRange: %+v", manifest)
	}
}

func writeBootstrapTree(t *testing.T, root, environment, namespace string) {
	t.Helper()
	overlay := filepath.Join(root, "overlays", environment)
	if err := os.MkdirAll(overlay, 0o755); err != nil {
		t.Fatal(err)
	}
	namespaceManifest := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + namespace + "\n"
	if err := os.WriteFile(filepath.Join(overlay, "namespace.yaml"), []byte(namespaceManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - namespace.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProjectResourceQuotaInjectsPromotableManifests(t *testing.T) {
	root := t.TempDir()
	writeBootstrapTree(t, root, "staging", "lodestar")
	projected, err := projectResourceQuota(root, "staging", "lodestar", fullResourceQuota())
	if err != nil {
		t.Fatal(err)
	}
	if !projected {
		t.Fatal("declared resource-quota was not projected")
	}
	overlay := filepath.Join(root, "overlays", "staging")
	kustomization, err := os.ReadFile(filepath.Join(overlay, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{resourceQuotaFile, limitRangeFile} {
		if !strings.Contains(string(kustomization), resource) {
			t.Fatalf("overlay kustomization does not reference %s: %s", resource, kustomization)
		}
	}

	kustomizer := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	rendered, err := kustomizer.Run(filesys.MakeFsOnDisk(), overlay)
	if err != nil {
		t.Fatalf("build bootstrap overlay: %v", err)
	}
	encoded, err := rendered.AsYaml()
	if err != nil {
		t.Fatal(err)
	}
	manifests, _, err := decodeYAML("overlay.yaml", encoded)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for index := range manifests {
		kinds[manifests[index].kind] = true
		// The Namespace in the fixture is cluster-scoped and needs an AppProject
		// contract the real render supplies; only the namespaced caps this test
		// projects are validated here.
		if manifests[index].kind != resourceQuotaKind && manifests[index].kind != limitRangeKind {
			continue
		}
		if err := validateManifest(manifests[index], nil, true); err != nil {
			t.Fatalf("projected %s is not promotable: %v", manifests[index].kind, err)
		}
	}
	if !kinds[resourceQuotaKind] || !kinds[limitRangeKind] {
		t.Fatalf("overlay rendered kinds = %v", kinds)
	}
}

func TestProjectResourceQuotaNoOpWithoutDeclaration(t *testing.T) {
	root := t.TempDir()
	writeBootstrapTree(t, root, "staging", "lodestar")
	projected, err := projectResourceQuota(root, "staging", "lodestar", nil)
	if err != nil {
		t.Fatal(err)
	}
	if projected {
		t.Fatal("projection ran without a declaration")
	}
	if _, err := os.Stat(filepath.Join(root, "overlays", "staging", resourceQuotaFile)); !os.IsNotExist(err) {
		t.Fatalf("%s was written without a declaration: %v", resourceQuotaFile, err)
	}
}

func TestProjectResourceQuotaRejectsMissingNamespace(t *testing.T) {
	root := t.TempDir()
	writeBootstrapTree(t, root, "staging", "lodestar")
	if _, err := projectResourceQuota(root, "staging", "", fullResourceQuota()); err == nil {
		t.Fatal("expected an error when the environment has no namespace")
	}
}
