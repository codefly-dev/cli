package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/imageevidence"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

func scannedImage(digest string) *builderv0.ImageSBOM {
	return &builderv0.ImageSBOM{
		Digest:   digest,
		Platform: "linux/amd64",
		Subjects: []*builderv0.ImageSubject{{Service: "users/accounts", Role: "app"}},
		Bom: &agentv0.Bom{
			BomFormat: "CycloneDX", SpecVersion: "1.5", Version: 1,
			Metadata:   &agentv0.Metadata{Component: &agentv0.Component{Name: "accounts", Version: "v1"}},
			Components: []*agentv0.Component{{Name: "libc", Version: "2.39"}},
		},
	}
}

func writePromotableUnit(t *testing.T, root, service, environment string) {
	t.Helper()
	overlay := filepath.Join(root, "services", service, "overlays", environment)
	if err := os.MkdirAll(overlay, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overlay, "deployment.yaml"), []byte(pinnedDeployment), 0o644); err != nil {
		t.Fatal(err)
	}
	kustomization := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - deployment.yaml\n"
	if err := os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), []byte(kustomization), 0o644); err != nil {
		t.Fatal(err)
	}
}

func publishUnitEvidence(t *testing.T, directory, digest string) {
	t.Helper()
	documents, err := imageevidence.Documents([]*builderv0.ImageSBOM{scannedImage(digest)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := imageevidence.Publish(directory, documents); err != nil {
		t.Fatal(err)
	}
}

func renderUnitWithEvidence(t *testing.T, evidence func(stage string)) (string, error) {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "deployments", "modules", "users")
	_, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: destination,
		Module:      "users",
		Environment: "local",
		Namespace:   "mind",
		AppProject:  "mind-users-local",
		Promotable:  true,
		OwnedPath:   "deployments/modules/users",
		Units:       promotableServiceGraph("users", []string{"accounts"}),
	}, func(_ context.Context, stage string) error {
		writePromotableUnit(t, stage, "accounts", "local")
		evidence(stage)
		return nil
	})
	return destination, err
}

// Evidence rides inside the unit whose manifests pin the digests it covers, so
// the promotion commit that moves those manifests moves their evidence with them.
func TestSnapshotAcceptsImageEvidenceInsideItsServiceUnit(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	destination, err := renderUnitWithEvidence(t, func(stage string) {
		publishUnitEvidence(t, filepath.Join(stage, "services", "accounts", imageEvidenceDir), digest)
	})
	if err != nil {
		t.Fatalf("render with in-unit image evidence failed: %v", err)
	}
	if err := ValidateServiceSnapshot(destination); err != nil {
		t.Fatalf("snapshot rejected evidence inside its own service unit: %v", err)
	}

	index := filepath.Join(destination, "services", "accounts", imageEvidenceDir, imageevidence.IndexFilename)
	if _, err := os.Stat(index); err != nil {
		t.Fatalf("published evidence index did not survive the render: %v", err)
	}
	inventory, err := LoadInventory(destination)
	if err != nil {
		t.Fatal(err)
	}
	covered := false
	for _, file := range inventory.Files {
		if strings.HasSuffix(file.Path, ".cdx.json") {
			covered = true
		}
	}
	if !covered {
		t.Fatalf("evidence document is not inventoried, so the render digest does not cover it: %+v", inventory.Files)
	}
}

// A service snapshot admits no path outside its unit graph, which is what forces
// evidence into the units rather than into one shared directory at the root.
func TestSnapshotRejectsImageEvidenceOutsideTheServiceGraph(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	destination, err := renderUnitWithEvidence(t, func(stage string) {
		publishUnitEvidence(t, filepath.Join(stage, imageEvidenceDir), digest)
	})
	if err != nil {
		t.Fatalf("render failed before the snapshot could be validated: %v", err)
	}
	err = ValidateServiceSnapshot(destination)
	if err == nil {
		t.Fatal("snapshot accepted evidence outside its service graph")
	}
	if !strings.Contains(err.Error(), "unexpected path") && !strings.Contains(err.Error(), "outside the exact service graph") {
		t.Fatalf("snapshot rejected root evidence for the wrong reason: %v", err)
	}
}
