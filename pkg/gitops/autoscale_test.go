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

func TestAutoscaleManifestRendersScaleTarget(t *testing.T) {
	manifest := autoscaleManifest("accounts", "payments", "accounts", &resources.ServiceAutoscale{Min: 2, Max: 6, TargetCPU: 70})
	if manifest.Kind != hpaKind || manifest.Metadata.Namespace != "payments" || manifest.Metadata.Name != "accounts" {
		t.Fatalf("manifest identity = %+v", manifest.Metadata)
	}
	if manifest.Spec.ScaleTargetRef.Kind != deploymentKind || manifest.Spec.ScaleTargetRef.Name != "accounts" {
		t.Fatalf("scaleTargetRef = %+v", manifest.Spec.ScaleTargetRef)
	}
	if manifest.Spec.MinReplicas != 2 || manifest.Spec.MaxReplicas != 6 {
		t.Fatalf("replica bounds = %d..%d", manifest.Spec.MinReplicas, manifest.Spec.MaxReplicas)
	}
	if len(manifest.Spec.Metrics) != 1 || manifest.Spec.Metrics[0].Resource.Target.AverageUtilization != 70 {
		t.Fatalf("metrics = %+v", manifest.Spec.Metrics)
	}
}

func TestServiceDeploymentNameDiscoversSoleDeployment(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	writeServiceTree(t, root, "production")
	name, err := serviceDeploymentName(root, "accounts")
	if err != nil {
		t.Fatal(err)
	}
	if name != "accounts" {
		t.Fatalf("discovered deployment = %q", name)
	}
}

func TestServiceDeploymentNameRejectsAmbiguousTree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	if err := os.MkdirAll(filepath.Join(root, "base"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := serviceDeploymentName(root, "accounts"); err == nil {
		t.Fatal("expected an error when the tree contains no Deployment")
	}
	two := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: one\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: two\n"
	if err := os.WriteFile(filepath.Join(root, "base", "deployments.yaml"), []byte(two), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := serviceDeploymentName(root, "accounts"); err == nil {
		t.Fatal("expected an error when the tree contains several Deployments")
	}
}

func TestProjectServiceAutoscaleInjectsPromotableOverlay(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	writeServiceTree(t, root, "production")
	projected, err := projectServiceAutoscale(root, "accounts", "production", "payments", &resources.ServiceAutoscale{Min: 2, Max: 6, TargetCPU: 70})
	if err != nil {
		t.Fatal(err)
	}
	if !projected {
		t.Fatal("declared autoscale was not projected")
	}
	overlay := filepath.Join(root, "overlays", "production")
	kustomization, err := os.ReadFile(filepath.Join(overlay, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(kustomization), hpaFile) {
		t.Fatalf("overlay kustomization does not reference the HPA: %s", kustomization)
	}

	kustomizer := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	rendered, err := kustomizer.Run(filesys.MakeFsOnDisk(), overlay)
	if err != nil {
		t.Fatalf("build service overlay: %v", err)
	}
	encoded, err := rendered.AsYaml()
	if err != nil {
		t.Fatal(err)
	}
	manifests, _, err := decodeYAML("overlay.yaml", encoded)
	if err != nil {
		t.Fatal(err)
	}
	var hpa *manifest
	for index := range manifests {
		if manifests[index].kind == hpaKind {
			hpa = &manifests[index]
		}
	}
	if hpa == nil {
		t.Fatalf("service overlay rendered no HorizontalPodAutoscaler: %d manifests", len(manifests))
	}
	if err := validateManifest(*hpa, nil, true); err != nil {
		t.Fatalf("projected HPA is not promotable: %v", err)
	}
}

func TestProjectServiceAutoscaleNoOpWithoutDeclaration(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	writeServiceTree(t, root, "production")
	projected, err := projectServiceAutoscale(root, "accounts", "production", "payments", nil)
	if err != nil {
		t.Fatal(err)
	}
	if projected {
		t.Fatal("projection ran without a declaration")
	}
	if _, err := os.Stat(filepath.Join(root, "overlays", "production", hpaFile)); !os.IsNotExist(err) {
		t.Fatalf("%s was written without a declaration: %v", hpaFile, err)
	}
}
