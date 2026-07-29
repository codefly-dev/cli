package gitops

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func TestCopySelectedEnvironmentBootstrapExcludesOtherEnvironments(t *testing.T) {
	module := t.TempDir()
	for _, environment := range []string{"local", "aws"} {
		root := filepath.Join(module, "deployment", "kustomize", "overlays", environment)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "kustomization.yaml"), []byte(environment+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	destination := t.TempDir()

	copied, err := copySelectedEnvironmentBootstrap(module, "local", destination)
	if err != nil {
		t.Fatal(err)
	}
	if !copied {
		t.Fatal("selected environment bootstrap was not copied")
	}
	data, err := os.ReadFile(filepath.Join(destination, "kustomize", "overlays", "local", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "local\n" {
		t.Fatalf("selected bootstrap = %q", data)
	}
	if _, err := os.Stat(filepath.Join(destination, "kustomize", "overlays", "aws")); !os.IsNotExist(err) {
		t.Fatalf("unselected bootstrap was copied: %v", err)
	}
}

func TestModuleRenderRootsCoverDependencyGraph(t *testing.T) {
	services := []*resources.Service{
		{Name: "frontend", ServiceDependencies: []*resources.ServiceDependency{{Name: "accounts"}}},
		{Name: "accounts", ServiceDependencies: []*resources.ServiceDependency{{Name: "store"}, {Name: "vault"}}},
		{Name: "forge-edge", ServiceDependencies: []*resources.ServiceDependency{{Name: "store"}, {Name: "vault"}}},
		{Name: "store"},
		{Name: "vault"},
	}

	roots, err := moduleRenderRoots("users", services)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, root := range roots {
		names = append(names, root.Name)
	}
	if got, want := names, []string{"frontend", "forge-edge"}; !slices.Equal(got, want) {
		t.Fatalf("roots = %v, want %v", got, want)
	}
}

func TestModuleRenderRootsRejectCycle(t *testing.T) {
	services := []*resources.Service{
		{Name: "one", ServiceDependencies: []*resources.ServiceDependency{{Name: "two"}}},
		{Name: "two", ServiceDependencies: []*resources.ServiceDependency{{Name: "one"}}},
	}

	if _, err := moduleRenderRoots("users", services); err == nil {
		t.Fatal("cyclic module graph was accepted")
	}
}
