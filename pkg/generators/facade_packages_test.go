package generators

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/languages"
)

// writeRaw writes files (slash-separated path → content) under a fresh
// directory: the shape proto.GenerateClient leaves for one entry.
func writeRaw(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for path, content := range files {
		target := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// rawGoEntry is what the Go facade plugin emits for one contract whose proto
// sits at the contract's root (a service's proto/api.proto): the binding at
// raw's root, and a facade, in a package named after the facade entry-point,
// importing it through the plugin's hardcoded prefix. Two such entries write
// the same binding path.
func rawGoEntry(t *testing.T, module string) string {
	t.Helper()
	return writeRaw(t, map[string]string{
		"api.pb.go":              "package " + module + "v1\n\ntype GetRequest struct{}\n",
		module + "_facade.pb.go": "package " + module + "\n\nimport (\n\t" + module + "v1 \"github.com/codefly-dev/cli/pkg/builder/clients/" + module + "\"\n)\n\ntype Request = " + module + "v1.GetRequest\n",
	})
}

func mergeGoFixture(t *testing.T, facadePackages bool) string {
	t.Helper()
	langDir := t.TempDir()
	const goModule = "example.test/portal-sdk-go"
	if err := os.WriteFile(filepath.Join(langDir, "go.mod"), []byte("module "+goModule+"\n\ngo "+goLanguageVersion+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, module := range []string{"accounts", "records"} {
		if err := mergeGoEntry(langDir, rawGoEntry(t, module), module, goModule, facadePackages); err != nil {
			t.Fatalf("mergeGoEntry %s: %v", module, err)
		}
	}
	return langDir
}

// TestAggregatedGoLibraryGivesEachFacadeItsOwnPackage is the aggregated Go
// library with two consumed modules: each facade carries its own package
// clause, so at the go/ root they are two packages in one directory and the
// library does not build; each in its own directory, it does.
func TestAggregatedGoLibraryGivesEachFacadeItsOwnPackage(t *testing.T) {
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOPROXY", "off")

	shared := mergeGoFixture(t, false)
	err := resolveAndVerifyGoLibrary(shared)
	if err == nil {
		t.Fatal("two entries merged into one go/ root and one go/gen/ must not build")
	}

	langDir := mergeGoFixture(t, true)
	for _, module := range []string{"accounts", "records"} {
		if _, err := os.Stat(filepath.Join(langDir, module, module+"_facade.pb.go")); err != nil {
			t.Fatalf("facade of %s is not in its own package go/%s/: %v", module, module, err)
		}
		if _, err := os.Stat(filepath.Join(langDir, "gen", module, "api.pb.go")); err != nil {
			t.Fatalf("bindings of %s are not under go/gen/%s/: %v", module, module, err)
		}
	}
	if err := resolveAndVerifyGoLibrary(langDir); err != nil {
		t.Fatalf("library with a package per facade does not build: %v", err)
	}

	// A regenerate clears the facade packages it wrote, and only what it wrote.
	note := filepath.Join(langDir, "records", "README.md")
	if err := os.WriteFile(note, []byte("hand-written\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := wipeGoLibrary(langDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(langDir, "accounts")); !os.IsNotExist(err) {
		t.Fatalf("wipe left the generated facade package go/accounts/ behind: %v", err)
	}
	if _, err := os.Stat(filepath.Join(langDir, "records", "records_facade.pb.go")); !os.IsNotExist(err) {
		t.Fatalf("wipe left a generated facade behind: %v", err)
	}
	if _, err := os.Stat(note); err != nil {
		t.Fatalf("wipe removed a hand-added file: %v", err)
	}
}

// rawTypeScriptEntry is what protoc-gen-es and the TypeScript facade plugin
// emit for one contract rooted at api.proto, in the proto package
// <module>.v1. Both fixture contracts write api_pb.ts and declare
// GetRequestSchema: one gen/ cannot hold both files.
func rawTypeScriptEntry(t *testing.T, module string) string {
	t.Helper()
	return writeRaw(t, map[string]string{
		"api_pb.ts": "// @generated from file api.proto (package " + module + ".v1, syntax proto3)\n" +
			"export const GetRequestSchema = {};\n" +
			"export const " + module + "Marker = {};\n",
		module + "_facade.ts": "import { GetRequestSchema } from \"./api_pb.js\";\nexport const " + module + " = { GetRequestSchema };\n",
	})
}

func mergeTypeScriptFixture(t *testing.T, facadePackages bool) string {
	t.Helper()
	langDir := t.TempDir()
	for _, module := range []string{"accounts", "records"} {
		if err := mergeTypeScriptEntry(langDir, rawTypeScriptEntry(t, module), module, facadePackages); err != nil {
			t.Fatalf("mergeTypeScriptEntry %s: %v", module, err)
		}
	}
	if _, err := finalizeTypeScriptLibrary(langDir, "@acme/portal-sdk"); err != nil {
		t.Fatalf("finalizeTypeScriptLibrary: %v", err)
	}
	return langDir
}

// TestAggregatedTypeScriptLibraryGivesEachEntryItsOwnBindings is the
// aggregated TypeScript library with two consumed modules: in one src/gen/
// the second contract's api_pb.ts replaces the first's, and the first facade
// imports a binding that is not its own. Under src/gen/<module>/ both stay,
// and the entry point is the namespaced barrel, which keeps the two
// contracts' same-named GetRequestSchema apart.
func TestAggregatedTypeScriptLibraryGivesEachEntryItsOwnBindings(t *testing.T) {
	read := func(langDir, path string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(langDir, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	shared := mergeTypeScriptFixture(t, false)
	if binding := read(shared, "src/gen/api_pb.ts"); strings.Contains(binding, "accountsMarker") {
		t.Fatalf("one src/gen/ kept both contracts' api_pb.ts; the fixture no longer shows the clash:\n%s", binding)
	}

	langDir := mergeTypeScriptFixture(t, true)
	for _, module := range []string{"accounts", "records"} {
		if binding := read(langDir, "src/gen/"+module+"/api_pb.ts"); !strings.Contains(binding, module+"Marker") {
			t.Errorf("bindings of %s are not under src/gen/%s/:\n%s", module, module, binding)
		}
		facade := read(langDir, "src/"+module+"_facade.ts")
		if !strings.Contains(facade, `from "./gen/`+module+`/api_pb.js"`) {
			t.Errorf("the %s facade does not import its own bindings:\n%s", module, facade)
		}
	}

	index := read(langDir, "src/index.ts")
	for _, line := range []string{
		`export * from "./accounts_facade.js";`,
		`export * from "./records_facade.js";`,
		`export * as accounts_v1_pb from "./packages/accounts.v1.js";`,
		`export * as records_v1_pb from "./packages/records.v1.js";`,
		`//   GetRequestSchema: accounts_v1_pb.GetRequestSchema, records_v1_pb.GetRequestSchema`,
	} {
		if !strings.Contains(index, line) {
			t.Errorf("src/index.ts lacks %q:\n%s", line, index)
		}
	}
	if packageBarrel := read(langDir, "src/packages/accounts.v1.ts"); !strings.Contains(packageBarrel, `export * from "../gen/accounts/api_pb.js";`) {
		t.Errorf("the accounts.v1 barrel does not reach its own bindings:\n%s", packageBarrel)
	}
}

// TestAggregatedLibraryRefusesTwoEntriesWithOneFacadeName: the facade name is
// a file and a package directory in every language, so the second entry
// would overwrite the first. Refused before any codegen.
func TestAggregatedLibraryRefusesTwoEntriesWithOneFacadeName(t *testing.T) {
	entry := func(module, pkg string) ContractEntry {
		return ContractEntry{
			ModuleName:          module,
			FacadeModuleDefault: "accounts",
			Endpoint: composition.APIContractEndpoint{
				Service: "accounts", Endpoint: "grpc", Kind: composition.APIContractKindProtobuf,
				Package: pkg, Digest: "sha256:" + module,
			},
		}
	}
	err := GenerateClientLibrary(context.Background(), &ClientLibraryRequest{
		Name:           "portal-sdk",
		Output:         t.TempDir(),
		Languages:      []languages.Language{languages.GO},
		Entries:        []ContractEntry{entry("host", "host.accounts.v1"), entry("other", "other.accounts.v1")},
		FacadePackages: true,
	})
	if err == nil || !strings.Contains(err.Error(), `both generate the facade "accounts"`) {
		t.Fatalf("error = %v, want the shared facade name refused", err)
	}
}
