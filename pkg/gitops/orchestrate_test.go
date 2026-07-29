package gitops

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func TestServiceRenderDestinationsKeepDependenciesInDistinctOwnedPaths(t *testing.T) {
	resolve := serviceRenderDestinations("/render")
	api := resolve(&resources.Module{Name: "payments"}, &resources.Service{Name: "api"})
	database := resolve(&resources.Module{Name: "platform"}, &resources.Service{Name: "postgres"})
	if api != filepath.Join("/render", "modules", "payments", "services", "api") {
		t.Fatalf("origin destination = %q", api)
	}
	if database != filepath.Join("/render", "modules", "platform", "services", "postgres") {
		t.Fatalf("dependency destination = %q", database)
	}
	if api == database {
		t.Fatal("origin and dependency render destinations collide")
	}
}

func TestCopyEnvironmentBootstrapCopiesOnlySelectedEnvironment(t *testing.T) {
	source := t.TempDir()
	for _, environment := range []string{"local", "aws"} {
		root := filepath.Join(source, "overlays", environment)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(root, "kustomization.yaml"),
			[]byte("resources:\n  - "+environment+".yaml\n"),
			0o644,
		); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, environment+".yaml"), []byte(pinnedDeployment), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	destination := filepath.Join(t.TempDir(), "bootstrap")
	if err := copyEnvironmentBootstrap(source, "local", destination); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "local.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "aws.yaml")); !os.IsNotExist(err) {
		t.Fatalf("unselected environment copied: %v", err)
	}
}
