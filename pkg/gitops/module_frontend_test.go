package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
)

// Exercise both delivery producers with an ordinary module, without a solution
// manifest. A reader-only test cannot detect a producer dropping this block.
func TestModuleFrontendReachesBothDeliveryPaths(t *testing.T) {
	dir := t.TempDir()
	entryDir := filepath.Join(dir, "services", "api")
	if err := os.MkdirAll(entryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"module.package.codefly.yaml":                  "kind: module-package\nschema: codefly/module-package/v2\nid: example/shared\nversion: 1.4.0\nminimum-codefly-version: '>=0.0.0'\nartifact-roots: [services]\ncontracts:\n  composition: '>=1.0'\n",
		"services/api/" + solutionrun.FrontendFileName: "title: Shared page\norder: 4\nexposed_module: ./Page\nhost_contract: 1\n",
		"services/api/binary":                          "native executable fixture",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	module := &resources.Module{Name: "shared", ServiceEntry: "api"}
	module.WithDir(dir)
	service := &resources.Service{Name: "api", Endpoints: []*resources.Endpoint{{Name: "http", API: "http", Visibility: "internal"}}}
	services := []*resources.Service{service}
	env := identityEnvironment()
	env.Namespace = "example"

	assertFrontend := func(t *testing.T, kind solutionhost.Kind, frontend *solutionhost.Frontend) {
		t.Helper()
		if kind != solutionhost.KindModule || frontend == nil || frontend.Title != "Shared page" || frontend.ExposedModule != "./Page" || frontend.HostContract != 1 {
			t.Fatalf("module frontend was lost or changed: kind=%s frontend=%+v", kind, frontend)
		}
	}
	t.Run("production", func(t *testing.T) {
		instance, undeclared, err := presenceInstanceOf(module, services, env, &RenderOptions{
			Module: "shared", Package: &InventoryPackage{ID: "example/shared", Version: "1.4.0"},
		})
		if err != nil || instance == nil || undeclared != "" {
			t.Fatalf("module presence: %+v, %q, %v", instance, undeclared, err)
		}
		assertFrontend(t, instance.Kind, instance.Frontend)
	})
	t.Run("native", func(t *testing.T) {
		deliverer := &NativeDeliverer{Workspace: &resources.Workspace{Name: "example"}, Env: env}
		documents, err := deliverer.Render(context.Background(), &NativeWorkload{
			Module: module, Service: service, Services: services,
			Subject: "system:serviceaccount:example:shared-api", SPIFFEID: "spiffe://cluster.example/ns/example/sa/shared-api",
			Executable: filepath.Join(entryDir, "binary"),
		})
		if err != nil {
			t.Fatal(err)
		}
		assertFrontend(t, documents.Presence.Kind, documents.Presence.Frontend)
	})
}
