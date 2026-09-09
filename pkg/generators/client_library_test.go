package generators

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	coreproto "github.com/codefly-dev/core/companions/proto"
	"github.com/codefly-dev/core/languages"
)

// TestManagedModeExceptsEveryMappedModule pins the coupling this repo cannot
// see: dropping a shared module's files from the image is only safe while the
// companion's Go template also tells managed mode to leave that module's
// go_package alone. The two halves live in different repositories, and they
// fail apart rather than together — if the except list loses an entry, buf
// still honors the import marker, still rewrites the go_package to the
// generated library's own path, and emits bindings importing a directory it
// no longer writes.
//
// Reading the template core actually renders is what makes a core bump that
// drops an entry fail here instead of in a published library.
func TestManagedModeExceptsEveryMappedModule(t *testing.T) {
	dir := t.TempDir()
	if err := coreproto.CreateBufConfiguration(context.Background(), dir, "svc", languages.GO, coreproto.FacadeOptions{Facade: true}); err != nil {
		t.Fatalf("CreateBufConfiguration: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "buf.gen.yaml"))
	if err != nil {
		t.Fatalf("read rendered buf.gen.yaml: %v", err)
	}
	template := string(data)

	for _, module := range upstreamProtoModules {
		if module.bufModule.isZero() {
			continue
		}
		if !strings.Contains(template, module.bufModule.identity()) {
			t.Errorf("%s is mapped upstream for %s but the companion's Go template does not except it:\n%s",
				module.bufModule.identity(), module.pathPrefix, template)
		}
	}
}

// TestResolveAndVerifyGoLibraryCompiles proves the generated Go library is
// actually built before the command reports success. A library whose bindings
// import a package that does not exist — what an unhonored module mapping
// produces — has to fail the command, because a published library version is
// immutable and cannot be repaired in place.
func TestResolveAndVerifyGoLibraryCompiles(t *testing.T) {
	// Keep the check hermetic: the fixtures import nothing outside the
	// standard library, so dependency resolution must never reach the network.
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOPROXY", "off")

	t.Run("valid library", func(t *testing.T) {
		dir := writeGoLibraryFixture(t, "package gen\n\nfunc Name() string { return \"ok\" }\n")
		if err := resolveAndVerifyGoLibrary(dir); err != nil {
			t.Fatalf("resolveAndVerifyGoLibrary: %v", err)
		}
	})

	t.Run("library that does not compile", func(t *testing.T) {
		dir := writeGoLibraryFixture(t, "package gen\n\nfunc Name() string { return missingPackage.Name }\n")
		err := resolveAndVerifyGoLibrary(dir)
		if err == nil {
			t.Fatal("resolveAndVerifyGoLibrary accepted a library that does not compile")
		}
		if !strings.Contains(err.Error(), "does not build") {
			t.Fatalf("error does not name the failure: %v", err)
		}
	})
}

func writeGoLibraryFixture(t *testing.T, source string) string {
	t.Helper()
	dir := t.TempDir()
	genDir := filepath.Join(dir, "gen")
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}
	goMod := "module example.test/lib\n\ngo " + goLanguageVersion + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(genDir, "gen.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestEnsurePythonDependencies pins that a library which stopped vendoring a
// shared module gains the distribution that supplies it — including on a
// regenerate, where the scaffold is never rewritten and would otherwise keep a
// dependency list that no longer covers the bindings' own imports — and that
// it does so without disturbing anything a developer added around it.
func TestEnsurePythonDependencies(t *testing.T) {
	dir := t.TempDir()
	original := `[project]
name = "accounts-client"
version = "0.0.1"
dependencies = ["protobuf>=5.29.3,<7"]

# hand-added below
[tool.pytest.ini_options]
addopts = "-q"
`
	path := filepath.Join(dir, "pyproject.toml")
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ensurePythonDependencies(dir, []string{"googleapis-common-protos", "protovalidate"}); err != nil {
		t.Fatalf("ensurePythonDependencies: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(after)
	for _, want := range []string{`"protobuf>=5.29.3,<7"`, `"googleapis-common-protos"`, `"protovalidate"`} {
		if !strings.Contains(got, want) {
			t.Errorf("pyproject.toml lost or never gained %s:\n%s", want, got)
		}
	}
	if !strings.Contains(got, `addopts = "-q"`) || !strings.Contains(got, "# hand-added below") {
		t.Errorf("hand-edited content was disturbed:\n%s", got)
	}

	// A regenerate must not keep appending the same packages.
	if err := ensurePythonDependencies(dir, []string{"googleapis-common-protos", "protovalidate"}); err != nil {
		t.Fatalf("second ensurePythonDependencies: %v", err)
	}
	twice, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(twice), `"protovalidate"`) != 1 {
		t.Fatalf("ensurePythonDependencies is not idempotent:\n%s", twice)
	}
}
