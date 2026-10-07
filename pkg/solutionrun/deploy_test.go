package solutionrun

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solution/manifest"
)

// A render derives the run's carrier under the run's name, as a public value:
// the projection names routes, never credentials, so it lands in the entry's
// ConfigMap, and nothing is rendered as a secret.
func TestDerivedDeployInputsProjectsConsumesAsAPublicValue(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")

	derived, err := DerivedDeployInputs(ctx, workspace)
	if err != nil {
		t.Fatalf("DerivedDeployInputs: %v", err)
	}

	solutionManifest, err := moduleManifest(wikiModuleIn(workspace))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]ServiceInjection{
		"wiki/backend": {Public: map[string]string{manifest.APIConsumesEnvironmentVariable: solutionManifest.ConsumedAPIsEnvValue()}},
	}
	if !reflect.DeepEqual(derived.Services, want) {
		t.Fatalf("services = %+v, want the projection on the entry alone: %+v", derived.Services, want)
	}
	for unique, injection := range derived.Services {
		for key := range injection.Public {
			if resources.IsSensitiveKey(key) {
				t.Errorf("%s renders the credential-named key %s as a value", unique, key)
			}
		}
	}
	if !slices.ContainsFunc(derived.Notes, func(note Note) bool {
		return !note.Warning && strings.Contains(note.Message, "wiki/backend") && strings.Contains(note.Message, manifest.APIConsumesEnvironmentVariable)
	}) {
		t.Errorf("notes = %+v, want the projection reported against the entry", derived.Notes)
	}
}

// A workspace composing no solution renders exactly as before.
func TestDerivedDeployInputsNoOpsWithoutASolution(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")
	workspace.Modules = slices.DeleteFunc(workspace.Modules,
		func(ref *resources.ModuleReference) bool { return ref.Name == "wiki" })

	derived, err := DerivedDeployInputs(ctx, workspace)
	if err != nil {
		t.Fatalf("DerivedDeployInputs: %v", err)
	}
	if len(derived.Services) != 0 || len(derived.Notes) != 0 {
		t.Fatalf("derived %+v for a workspace with no solution", derived)
	}
}

// A solution that consumes nothing has nothing to project, so its entry is
// handed nothing rather than an empty projection.
func TestDerivedDeployInputsNoOpsWithoutConsumes(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, federationWorkspaceWithManifest(t, solutionManifestWithoutConsumes))

	derived, err := DerivedDeployInputs(ctx, workspace)
	if err != nil {
		t.Fatalf("DerivedDeployInputs: %v", err)
	}
	if len(derived.Services) != 0 {
		t.Fatalf("derived %+v for a solution that consumes nothing", derived.Services)
	}
}

// A manifest the run would refuse fails the render the same way, naming the
// solution: a render that silently projected nothing would ship a deployment
// whose consumed routes stay unrouted.
func TestDerivedDeployInputsRefusesAManifestTheRunWouldRefuse(t *testing.T) {
	ctx := context.Background()
	duplicated := strings.Replace(solutionManifestWithConsumes, `lifecycle:`, `    - id: archives
      protocol: connect
      module: archives
      service: api
      endpoint: connect
      as: documents
lifecycle:`, 1)
	workspace := loadTestWorkspace(t, federationWorkspaceWithManifest(t, duplicated))

	_, err := DerivedDeployInputs(ctx, workspace)
	if err == nil || !strings.Contains(err.Error(), "solution wiki") || !strings.Contains(err.Error(), "documents") {
		t.Fatalf("DerivedDeployInputs = %v, want a refusal naming the solution and the duplicated prefix", err)
	}
}

// federationWorkspaceWithManifest copies the federation testdata with the wiki
// solution's manifest replaced.
func federationWorkspaceWithManifest(t *testing.T, solutionManifest string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("testdata/solution-federation")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), []byte(solutionManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}
