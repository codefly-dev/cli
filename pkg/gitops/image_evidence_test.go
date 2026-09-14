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
	"github.com/codefly-dev/core/resources"
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
	if _, err := imageevidence.Publish(directory, documents, true); err != nil {
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
		publishUnitEvidence(t, filepath.Join(stage, "services", "accounts", filepath.FromSlash(imageEvidenceDir)), digest)
	})
	if err != nil {
		t.Fatalf("render with in-unit image evidence failed: %v", err)
	}
	if err := ValidateServiceSnapshot(destination); err != nil {
		t.Fatalf("snapshot rejected evidence inside its own service unit: %v", err)
	}

	index := filepath.Join(destination, "services", "accounts", filepath.FromSlash(imageEvidenceDir), imageevidence.IndexFilename)
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
		publishUnitEvidence(t, filepath.Join(stage, filepath.FromSlash(imageEvidenceDir)), digest)
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

func evidenceRequest(stage string, origin *resources.Service, managed map[string]bool) *serviceFlowRequest {
	return &serviceFlowRequest{
		service: origin,
		env:     &resources.Environment{Name: "production"},
		destination: func(_ *resources.Module, service *resources.Service) string {
			return filepath.Join(stage, "services", service.Name)
		},
		managed: managed,
	}
}

func unitEvidenceIndex(stage, service string) string {
	return filepath.Join(stage, "services", service, filepath.FromSlash(imageEvidenceDir), imageevidence.IndexFilename)
}

// retainManagedBundle removes or wholesale-replaces a managed service's rendered
// directory after the flow has run, so evidence written there is destroyed. The
// render already pushed that image, so publishing and reporting success would
// claim coverage that no longer exists on disk.
func TestSnapshotEvidenceRefusesAManagedServiceWhoseDirectoryIsReplaced(t *testing.T) {
	stage := t.TempDir()
	postgres := &resources.Service{Name: "postgres"}
	postgres.WithModule("platform")
	request := evidenceRequest(stage, postgres, map[string]bool{"postgres": true})
	resolve := func(string) (*resources.Service, error) { return postgres, nil }

	err := publishImageEvidenceInto(
		map[string][]*builderv0.ImageSBOM{"platform/postgres": {scannedImage("sha256:" + strings.Repeat("a", 64))}},
		[]string{"platform/postgres"}, resolve, request,
	)
	if err == nil {
		t.Fatal("evidence was published into a managed unit that is replaced right after")
	}
	if !strings.Contains(err.Error(), "managed") {
		t.Fatalf("error does not name the managed unit as the cause: %v", err)
	}
	if _, statErr := os.Stat(unitEvidenceIndex(stage, "postgres")); !os.IsNotExist(statErr) {
		t.Fatalf("evidence was written for a managed service: %v", statErr)
	}
}

// An absent directory cannot be told apart from collection that never ran, so a
// render owing evidence for no image has to record that explicitly.
func TestSnapshotEvidenceRecordsAnEmptyIndexWhenNothingWasCollected(t *testing.T) {
	stage := t.TempDir()
	accounts := &resources.Service{Name: "accounts"}
	accounts.WithModule("users")
	request := evidenceRequest(stage, accounts, nil)
	resolve := func(string) (*resources.Service, error) { return accounts, nil }

	if err := publishImageEvidenceInto(nil, []string{"users/accounts"}, resolve, request); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(unitEvidenceIndex(stage, "accounts"))
	if err != nil {
		t.Fatalf("a render that collected nothing recorded nothing: %v", err)
	}
	if !strings.Contains(string(payload), `"images": []`) {
		t.Fatalf("empty evidence is not an explicit empty list: %s", payload)
	}
}
