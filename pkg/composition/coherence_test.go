package composition

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

// fakeResolver resolves names+constraints to preset versions, driving the
// closure walk from constructed libraries. It lets the coherence logic be
// tested against true multi-major diamonds without git-tagged fixtures.
type fakeResolver struct {
	libs     map[string]*resources.Library
	versions map[string]string // "name@constraint" -> resolved version
}

func (f *fakeResolver) ResolveVersion(_ context.Context, name, constraint string) (*resources.Library, string, error) {
	lib, ok := f.libs[name]
	if !ok {
		return nil, "", fmt.Errorf("unknown library %s", name)
	}
	resolved, ok := f.versions[name+"@"+constraint]
	if !ok {
		return nil, "", fmt.Errorf("no version of %s satisfies %s", name, constraint)
	}
	return lib, resolved, nil
}

func lib(name string, deps ...*resources.LibraryReference) *resources.Library {
	return &resources.Library{Name: name, LibraryDeps: deps}
}

func ref(name, version string) *resources.LibraryReference {
	return &resources.LibraryReference{Name: name, Version: version}
}

func dep(name, version string) *resources.LibraryDependency {
	return &resources.LibraryDependency{Name: name, Version: version, Languages: []string{"go"}}
}

func TestClosureCoherentSharedLibrary(t *testing.T) {
	fake := &fakeResolver{
		libs: map[string]*resources.Library{
			"accounts-sdk": lib("accounts-sdk", ref("common", "^1.0.0")),
			"billing-sdk":  lib("billing-sdk", ref("common", "^1.0.0")),
			"common":       lib("common"),
		},
		versions: map[string]string{
			"accounts-sdk@^1.0.0": "1.4.0",
			"billing-sdk@^1.0.0":  "1.1.0",
			"common@^1.0.0":       "1.2.0",
		},
	}
	r := &Resolver{resolve: fake}

	closure, err := r.Closure(context.Background(), []*resources.LibraryDependency{
		dep("accounts-sdk", "^1.0.0"),
		dep("billing-sdk", "^1.0.0"),
	}, "solution accounts+billing")
	if err != nil {
		t.Fatalf("closure: %v", err)
	}

	// common is reached through both SDKs, so both requirements are recorded.
	if got := len(closure.Requirements["common"]); got != 2 {
		t.Fatalf("common requirements = %d, want 2", got)
	}
	if err := closure.Validate(); err != nil {
		t.Fatalf("coherent closure rejected: %v", err)
	}
}

func TestClosureDiamondAcrossMajors(t *testing.T) {
	fake := &fakeResolver{
		libs: map[string]*resources.Library{
			"accounts-sdk": lib("accounts-sdk", ref("common", "^1.0.0")),
			"billing-sdk":  lib("billing-sdk", ref("common", "^2.0.0")),
			"common":       lib("common"),
		},
		versions: map[string]string{
			"accounts-sdk@^1.0.0": "1.4.0",
			"billing-sdk@^1.0.0":  "1.1.0",
			"common@^1.0.0":       "1.2.0",
			"common@^2.0.0":       "2.1.0",
		},
	}
	r := &Resolver{resolve: fake}

	closure, err := r.Closure(context.Background(), []*resources.LibraryDependency{
		dep("accounts-sdk", "^1.0.0"),
		dep("billing-sdk", "^1.0.0"),
	}, "solution accounts+billing")
	if err != nil {
		t.Fatalf("closure: %v", err)
	}

	violations, err := closure.Violations()
	if err != nil {
		t.Fatalf("violations: %v", err)
	}
	if len(violations) != 1 || violations[0].Library != "common" {
		t.Fatalf("violations = %#v, want one for common", violations)
	}
	if len(violations[0].Majors) != 2 {
		t.Fatalf("common majors = %d, want 2", len(violations[0].Majors))
	}

	err = closure.Validate()
	if err == nil {
		t.Fatal("diamond closure was accepted")
	}
	for _, want := range []string{"common", "v1", "v2", "accounts-sdk", "billing-sdk"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestClosureUnresolvableDependencyErrors(t *testing.T) {
	fake := &fakeResolver{
		libs:     map[string]*resources.Library{"accounts-sdk": lib("accounts-sdk", ref("common", "^9.0.0"))},
		versions: map[string]string{"accounts-sdk@^1.0.0": "1.0.0"},
	}
	r := &Resolver{resolve: fake}

	_, err := r.Closure(context.Background(), []*resources.LibraryDependency{dep("accounts-sdk", "^1.0.0")}, "solution")
	if err == nil {
		t.Fatal("expected error resolving unsatisfiable transitive dependency")
	}
	if !strings.Contains(err.Error(), "common") {
		t.Fatalf("error %q missing offending library", err.Error())
	}
}

func TestClosureTerminatesOnCycle(t *testing.T) {
	fake := &fakeResolver{
		libs: map[string]*resources.Library{
			"a": lib("a", ref("b", "^1.0.0")),
			"b": lib("b", ref("a", "^1.0.0")),
		},
		versions: map[string]string{"a@^1.0.0": "1.0.0", "b@^1.0.0": "1.0.0"},
	}
	r := &Resolver{resolve: fake}

	closure, err := r.Closure(context.Background(), []*resources.LibraryDependency{dep("a", "^1.0.0")}, "solution")
	if err != nil {
		t.Fatalf("closure: %v", err)
	}
	if err := closure.Validate(); err != nil {
		t.Fatalf("cyclic-but-coherent closure rejected: %v", err)
	}
}

// TestClosureAgainstLocalWorkspace exercises the real LibraryResolver over an
// on-disk workspace, covering the coherent path and an unsatisfiable one.
func TestClosureAgainstLocalWorkspace(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, resources.WorkspaceConfigurationName), "name: composition-fixture\nlayout: flat\n")

	writeLibrary(t, root, "common", "1.2.0")
	writeLibrary(t, root, "accounts-sdk", "1.0.0", ref("common", "^1.0.0"))
	writeLibrary(t, root, "billing-sdk", "1.0.0", ref("common", "^1.0.0"))
	writeLibrary(t, root, "legacy-sdk", "1.0.0", ref("common", "^0.1.0"))

	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	r := NewResolver(workspace)

	closure, err := r.Closure(context.Background(), []*resources.LibraryDependency{
		dep("accounts-sdk", "^1.0.0"),
		dep("billing-sdk", "^1.0.0"),
	}, "solution")
	if err != nil {
		t.Fatalf("closure: %v", err)
	}
	if err := closure.Validate(); err != nil {
		t.Fatalf("coherent local closure rejected: %v", err)
	}

	if _, err := r.Closure(context.Background(), []*resources.LibraryDependency{
		dep("legacy-sdk", "^1.0.0"),
	}, "solution"); err == nil {
		t.Fatal("expected error: legacy-sdk needs common ^0.1.0 but only 1.2.0 exists")
	}
}

func writeLibrary(t *testing.T, root, name, version string, deps ...*resources.LibraryReference) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "kind: library\nname: %s\nversion: %s\nlanguages:\n    - name: go\n      agent: \"\"\n      path: go/\n      exports: [example/%s]\n", name, version, name)
	if len(deps) > 0 {
		b.WriteString("library-dependencies:\n")
		for _, d := range deps {
			fmt.Fprintf(&b, "    - name: %s\n      version: %s\n", d.Name, d.Version)
		}
	}
	writeFixture(t, filepath.Join(root, "libraries", name, resources.LibraryConfigurationName), b.String())
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
