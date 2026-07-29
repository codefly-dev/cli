package gitops

import (
	"os"
	"path/filepath"
	"testing"
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
