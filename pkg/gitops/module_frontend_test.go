package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// A solution is held to the file a module may omit: without it the host would
// list nothing and the runtime would not boot, so both producers refuse it.
func TestASolutionWithoutItsFrontendIsRefusedByBothDeliveryPaths(t *testing.T) {
	module := solutionModule(t, "crm", "api")
	if err := os.Remove(filepath.Join(module.Dir(), "services", "api", solutionrun.FrontendFileName)); err != nil {
		t.Fatal(err)
	}
	// The native delivery digests the started executable and reads the package
	// manifest before it reads the declaration; the refusal under test is the
	// declaration's.
	for name, contents := range map[string]string{
		"services/api/binary":         "native executable fixture",
		"module.package.codefly.yaml": "kind: module-package\nschema: codefly/module-package/v2\nid: example/crm\nversion: 1.0.0\nminimum-codefly-version: '>=0.0.0'\nartifact-roots: [services]\ncontracts:\n  composition: '>=1.0'\n",
	} {
		if err := os.WriteFile(filepath.Join(module.Dir(), name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	service := &resources.Service{Name: "api", Endpoints: []*resources.Endpoint{{Name: "http", API: "http", Visibility: "internal"}}}
	env := identityEnvironment()
	options := &RenderOptions{Module: "crm", Package: &InventoryPackage{ID: "example/crm", Version: "1.0.0"}}
	if _, _, err := presenceInstanceOf(module, []*resources.Service{service}, env, options); err == nil || !strings.Contains(err.Error(), "declares no frontend") {
		t.Fatalf("render accepted a solution with no frontend: %v", err)
	}
	deliverer := &NativeDeliverer{Workspace: &resources.Workspace{Name: "example"}, Env: env}
	_, err := deliverer.Render(context.Background(), &NativeWorkload{
		Module: module, Service: service, Services: []*resources.Service{service},
		Subject: "system:serviceaccount:example:crm-api", SPIFFEID: "spiffe://cluster.example/ns/example/sa/crm-api",
		Executable: filepath.Join(module.Dir(), "services", "api", "binary"),
	})
	if err == nil || !strings.Contains(err.Error(), "declares no frontend") {
		t.Fatalf("native delivery accepted a solution with no frontend: %v", err)
	}
}
