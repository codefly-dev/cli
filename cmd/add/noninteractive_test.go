package add

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/resources"
)

// addTestWorkspace writes a workspace with three modules: app (services api
// and store), other (service store, exported by the module's interface) and
// closed (service store, not exported), and moves the test into it. The tests run
// without a terminal, as CI, a script or an MCP client does.
func addTestWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeAddTestFile(t, filepath.Join(root, resources.WorkspaceConfigurationName),
		"name: test\nlayout: modules\nmodules:\n    - name: app\n    - name: other\n    - name: closed\n")
	service := func(module, name string) {
		writeAddTestFile(t, filepath.Join(root, "modules", module, "services", name, "service.codefly.yaml"),
			"name: "+name+"\nversion: 0.0.0\nagent:\n  kind: codefly:service\n  name: fixture\n  publisher: codefly.dev\n  version: 1.0.0\nendpoints:\n    - name: tcp\n")
	}
	writeAddTestFile(t, filepath.Join(root, "modules", "app", "module.codefly.yaml"),
		"kind: module\nname: app\nservices:\n    - name: api\n    - name: store\n")
	writeAddTestFile(t, filepath.Join(root, "modules", "other", "module.codefly.yaml"),
		"kind: module\nname: other\nservices:\n    - name: store\ninterface:\n    endpoints:\n        - service: store\n          endpoint: tcp\n")
	writeAddTestFile(t, filepath.Join(root, "modules", "closed", "module.codefly.yaml"),
		"kind: module\nname: closed\nservices:\n    - name: store\n")
	service("app", "api")
	service("app", "store")
	service("other", "store")
	service("closed", "store")
	t.Chdir(root)
	t.Cleanup(func() { cli.SetWithDefault(false) })
	return root
}

func setDependencyFlags(t *testing.T, target string, yes bool) {
	t.Helper()
	previousTarget, previousYes := serviceDependencyTarget, serviceDependencyYes
	serviceDependencyTarget, serviceDependencyYes = target, yes
	t.Cleanup(func() { serviceDependencyTarget, serviceDependencyYes = previousTarget, previousYes })
}

func dependenciesOf(t *testing.T, root, module, service string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "modules", module, "services", service, "service.codefly.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, after, found := strings.Cut(string(data), "service-dependencies:")
	if !found {
		return ""
	}
	return after
}

// A dependency named with --dependency and confirmed with --yes is added
// without a terminal: in the service's own module by default, in another
// module when qualified.
func TestAddServiceDependencyWithoutATerminal(t *testing.T) {
	root := addTestWorkspace(t)

	setDependencyFlags(t, "store", true)
	if err := addServiceDependency(context.Background(), []string{"app/api"}); err != nil {
		t.Fatalf("same-module dependency: %v", err)
	}
	if deps := dependenciesOf(t, root, "app", "api"); !strings.Contains(deps, "name: store") || strings.Contains(deps, "module: other") {
		t.Fatalf("app/api should depend on its own module's store:\n%s", deps)
	}

	setDependencyFlags(t, "other/store", true)
	if err := addServiceDependency(context.Background(), []string{"app/api"}); err != nil {
		t.Fatalf("cross-module dependency: %v", err)
	}
	if deps := dependenciesOf(t, root, "app", "api"); !strings.Contains(deps, "module: other") {
		t.Fatalf("app/api should also depend on other/store:\n%s", deps)
	}
}

// What cannot be linked is refused by name, before anything is written.
func TestAddServiceDependencyRefusesWhatDoesNotExist(t *testing.T) {
	root := addTestWorkspace(t)
	for name, tc := range map[string]struct{ target, want string }{
		"an unknown service": {"nope", "has no service nope"},
		// The module boundary still decides: naming a service does not reach
		// an endpoint its module does not export.
		"a service its module does not export": {"closed/store", "does not permit module"},
		"an unknown module":                    {"ghost/store", "ghost"},
		"itself":                               {"api", "cannot depend on itself"},
		"a malformed name":                     {"a/b/c", "--dependency is a service or module/service"},
	} {
		setDependencyFlags(t, tc.target, true)
		err := addServiceDependency(context.Background(), []string{"app/api"})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want it to contain %q", name, err, tc.want)
		}
	}
	if deps := dependenciesOf(t, root, "app", "api"); deps != "" {
		t.Fatalf("a refused dependency was written:\n%s", deps)
	}
}

// Without --yes and without a terminal the confirmation cannot be asked: the
// command fails saying so instead of reporting a decline, and writes nothing.
func TestAddServiceDependencyWithoutYesSaysWhatToPass(t *testing.T) {
	root := addTestWorkspace(t)
	setDependencyFlags(t, "store", false)
	err := addServiceDependency(context.Background(), []string{"app/api"})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error = %v, want it to name --yes", err)
	}
	if deps := dependenciesOf(t, root, "app", "api"); deps != "" {
		t.Fatalf("an unconfirmed dependency was written:\n%s", deps)
	}
}

// A library is created without a terminal when --yes is passed.
func TestAddLibraryWithYesNeedsNoTerminal(t *testing.T) {
	root := addTestWorkspace(t)
	previousYes, previousLanguages := libraryYes, libraryLanguages
	libraryYes, libraryLanguages = true, []string{"go"}
	t.Cleanup(func() { libraryYes, libraryLanguages = previousYes, previousLanguages })

	if err := addLibrary("shared"); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(root, "libraries", "shared", "*.yaml"))
	if len(matches) == 0 {
		t.Fatalf("no library was created under %s", filepath.Join(root, "libraries", "shared"))
	}
}
