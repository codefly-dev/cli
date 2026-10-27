package generators

import (
	"context"
	"encoding/json"
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

// TestTypeScriptFacadeImportsReachIntoGen pins the specifier the facade
// rewrite produces. The facade is hoisted out of the generated tree to src/,
// so its same-directory imports must name the binding's new place under gen/
// — and for a facade at the tree's root (a proto file with no package-derived
// subdirectory, e.g. api.proto), that place is gen/ itself, not "gen/./".
func TestTypeScriptFacadeImportsReachIntoGen(t *testing.T) {
	for _, tc := range []struct {
		name, facadeDir, want string
	}{
		{name: "root facade", facadeDir: "", want: `from "./gen/api_pb.js"`},
		{name: "package facade", facadeDir: "saas/accounts/v1", want: `from "./gen/saas/accounts/v1/api_pb.js"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, langDir := t.TempDir(), t.TempDir()
			dir := filepath.Join(raw, filepath.FromSlash(tc.facadeDir))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			facade := "import { Pdf } from \"./api_pb.js\";\n"
			if err := os.WriteFile(filepath.Join(dir, "pdf_facade.ts"), []byte(facade), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "api_pb.ts"), []byte("export const Pdf = {};\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := mergeTypeScriptEntry(langDir, raw, "", false); err != nil {
				t.Fatalf("mergeTypeScriptEntry: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(langDir, "src", "pdf_facade.ts"))
			if err != nil {
				t.Fatalf("read hoisted facade: %v", err)
			}
			if !strings.Contains(string(got), tc.want) {
				t.Errorf("hoisted facade imports\n%s\nwant a specifier %s", got, tc.want)
			}
			binding := filepath.Join(langDir, "src", "gen", filepath.FromSlash(tc.facadeDir), "api_pb.ts")
			if _, err := os.Stat(binding); err != nil {
				t.Errorf("the binding the specifier names is not where it points: %v", err)
			}
		})
	}
}

// TestEnsurePythonDependenciesKeepsADevelopersOwnPin pins that a package is
// declared by its distribution name, not by the exact requirement string: a
// developer who pinned grpcio or protovalidate themselves keeps that pin, and
// the library never gains a second, conflicting requirement for it.
func TestEnsurePythonDependenciesKeepsADevelopersOwnPin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pyproject.toml")
	original := `[project]
dependencies = ["protobuf>=5.29.3,<7", "grpcio==1.75.1", "Protovalidate>=0.7"]
`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensurePythonDependencies(dir, []string{pythonGrpcRequirement, "protovalidate", "googleapis-common-protos"}); err != nil {
		t.Fatalf("ensurePythonDependencies: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `dependencies = ["protobuf>=5.29.3,<7", "grpcio==1.75.1", "Protovalidate>=0.7", "googleapis-common-protos"]`
	if !strings.Contains(string(got), want) {
		t.Fatalf("got:\n%s\nwant the line:\n%s", got, want)
	}
}

func TestPythonDistribution(t *testing.T) {
	for requirement, want := range map[string]string{
		"grpcio>=1.71.0":                   "grpcio",
		"protobuf>=5.29.3,<7":              "protobuf",
		"googleapis-common-protos":         "googleapis-common-protos",
		"Googleapis_Common_Protos==1.66":   "googleapis-common-protos",
		"protovalidate ~= 0.7":             "protovalidate",
		"grpcio[protobuf]>=1.71":           "grpcio",
		`grpcio; python_version >= "3.10"`: "grpcio",
		"opentelemetry-proto!=1.2":         "opentelemetry-proto",
	} {
		if got := pythonDistribution(requirement); got != want {
			t.Errorf("pythonDistribution(%q) = %q, want %q", requirement, got, want)
		}
	}
}

// TestPythonScaffoldDeclaresGrpcio pins that a new library depends on the
// grpcio its *_pb2_grpc stubs check at import, the floor the companion's
// grpc_tools sets.
func TestPythonScaffoldDeclaresGrpcio(t *testing.T) {
	dir := t.TempDir()
	if err := writePythonScaffold(dir, "authority_client"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "pyproject.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `dependencies = ["protobuf>=5.29.3,<7", "grpcio>=1.71.0"]`) {
		t.Fatalf("scaffold does not declare grpcio:\n%s", got)
	}
}

// TestTypeScriptLibraryDeclaresAnEntryPointItProduces pins the two halves of
// the generated package that must agree: package.json's "exports" and the file
// tree. They lived in different functions and disagreed — the scaffold named
// ./src/index.ts and nothing ever wrote it, so every generated TypeScript
// library was unresolvable by the name it published under. A barrel that is
// not regenerated is the same bug one run later, since src/ is wiped per run.
func TestTypeScriptLibraryDeclaresAnEntryPointItProduces(t *testing.T) {
	langDir := t.TempDir()
	genDir := filepath.Join(langDir, "src", "gen")
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// What buf leaves behind: bindings under gen/, a facade hoisted beside it.
	for path, content := range map[string]string{
		filepath.Join(genDir, "api_pb.ts"):             "export const Pdf = {};\n",
		filepath.Join(langDir, "src", "pdf_facade.ts"): "export const pdf = {};\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	export, err := finalizeTypeScriptLibrary(langDir, "@acme/pdf-client")
	if err != nil {
		t.Fatalf("finalizeTypeScriptLibrary: %v", err)
	}
	if export.Name != "typescript" {
		t.Fatalf("export name = %q, want typescript", export.Name)
	}

	manifest, err := os.ReadFile(filepath.Join(langDir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		Exports map[string]struct {
			Types  string `json:"types"`
			Import string `json:"import"`
		} `json:"exports"`
		Files   []string          `json:"files"`
		Scripts map[string]string `json:"scripts"`
	}
	if err = json.Unmarshal(manifest, &pkg); err != nil {
		t.Fatalf("package.json is not valid JSON: %v\n%s", err, manifest)
	}

	// The entry point must be something tsconfig emits, "files" must ship it,
	// and a build script must exist to produce it.
	root, ok := pkg.Exports["."]
	if !ok {
		t.Fatalf(`package.json has no "." export: %s`, manifest)
	}
	for _, declared := range []string{root.Import, root.Types} {
		if !strings.HasPrefix(declared, "./dist/") {
			t.Errorf("entry %q is not under ./dist/, which is tsconfig's outDir and the only thing %v ships", declared, pkg.Files)
		}
	}
	if pkg.Scripts["build"] == "" {
		t.Error(`package.json declares a dist/ entry point but no "build" script to emit it`)
	}
	shipped := false
	for _, pattern := range pkg.Files {
		if pattern == "dist" {
			shipped = true
		}
	}
	if !shipped {
		t.Errorf(`"files" %v does not ship dist/, so the published package would omit its own entry point`, pkg.Files)
	}

	// The barrel itself: present, and reaching every generated module.
	index, err := os.ReadFile(filepath.Join(langDir, "src", "index.ts"))
	if err != nil {
		t.Fatalf("src/index.ts, the file tsconfig compiles into the declared entry point, was not written: %v", err)
	}
	for _, want := range []string{`export * from "./gen/api_pb.js";`, `export * from "./pdf_facade.js";`} {
		if !strings.Contains(string(index), want) {
			t.Errorf("src/index.ts does not re-export %s\ngot:\n%s", want, index)
		}
	}

	// Regenerating must rewrite the barrel, not leave a stale one: src/ is
	// wiped per run, so a write-once index would vanish on the second run.
	if err = wipeTypeScriptLibrary(langDir); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(genDir, "other_pb.ts"), []byte("export const Other = {};\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = finalizeTypeScriptLibrary(langDir, "@acme/pdf-client"); err != nil {
		t.Fatalf("second finalizeTypeScriptLibrary: %v", err)
	}
	index, err = os.ReadFile(filepath.Join(langDir, "src", "index.ts"))
	if err != nil {
		t.Fatalf("src/index.ts was not rewritten after a wipe: %v", err)
	}
	if !strings.Contains(string(index), `export * from "./gen/other_pb.js";`) {
		t.Errorf("the regenerated barrel missed the new module:\n%s", index)
	}
	if strings.Contains(string(index), "api_pb") {
		t.Errorf("the regenerated barrel still re-exports a module the wipe removed:\n%s", index)
	}
}
