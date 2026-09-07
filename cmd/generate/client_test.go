package generate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	coreproto "github.com/codefly-dev/core/companions/proto"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
)

func resetClientFlags(t *testing.T) {
	t.Helper()
	prev := struct {
		from, name, moduleName, output, endpoint, goModule, npmScope string
		languages, servicesFlag                                      []string
		noFacade, force                                              bool
	}{
		clientFrom, clientName, clientModuleName, clientOutput, clientEndpoint, clientGoModule, clientNpmScope,
		clientLanguages, clientServices, clientNoFacade, clientForce,
	}
	clientFrom, clientLanguages, clientServices = "", nil, nil
	clientName, clientModuleName, clientOutput, clientEndpoint = "", "", "", ""
	clientGoModule, clientNpmScope = "", ""
	clientNoFacade, clientForce = false, false
	t.Cleanup(func() {
		clientFrom, clientName, clientModuleName, clientOutput, clientEndpoint, clientGoModule, clientNpmScope =
			prev.from, prev.name, prev.moduleName, prev.output, prev.endpoint, prev.goModule, prev.npmScope
		clientLanguages, clientServices = prev.languages, prev.servicesFlag
		clientNoFacade, clientForce = prev.noFacade, prev.force
	})
}

// captureStderr runs fn while collecting everything written to os.Stderr.
func captureStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan string)
	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()
	runErr := fn()
	os.Stderr = old
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out, runErr
}

func requireQualify(t *testing.T) {
	t.Helper()
	if os.Getenv("CODEFLY_GENERATE_QUALIFY") != "1" {
		t.Skip("set CODEFLY_GENERATE_QUALIFY=1 to run the disposable Docker qualification")
	}
}

func TestGenerateClientFromModuleService(t *testing.T) {
	requireQualify(t)
	moduleDir := scaffoldGoGRPCFixture(t)
	wsDir := filepath.Dir(moduleDir)

	t.Chdir(wsDir)
	resetClientFlags(t)
	clientFrom = "billing/api/grpc"
	clientLanguages = []string{"go"}
	// The local source generates from the service's own proto sources (see
	// resolvedContractSource.protoSources), so --services works here; a
	// contracts:/package: source only has the persisted descriptor bytes and
	// hits a core bug filtering --services over a multi-file descriptor set
	// (tracked, not something this repo can fix) — see
	// TestGenerateClientFromContractsDir, which does not set --services.
	clientServices = []string{"ApiService"}
	clientOutput = filepath.Join(t.TempDir(), "lib")

	if err := ClientCmd.RunE(ClientCmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	assertLibraryLayout(t, clientOutput, "go")
	goVetLibrary(t, filepath.Join(clientOutput, "go"))
}

func TestGenerateClientFromContractsDir(t *testing.T) {
	requireQualify(t)
	moduleDir := scaffoldGoGRPCFixture(t)

	t.Chdir(moduleDir)
	resetContractsFlags(t)
	if err := ContractsCmd.RunE(ContractsCmd, []string{"billing"}); err != nil {
		t.Fatalf("generate contracts: %v", err)
	}

	resetClientFlags(t)
	clientFrom = "contracts:" + filepath.Join(moduleDir, "contracts", "api")
	clientLanguages = []string{"go", "typescript", "python"}
	clientName = "audit-client"
	clientOutput = filepath.Join(t.TempDir(), "lib")

	if err := ClientCmd.RunE(ClientCmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	manifestData, err := os.ReadFile(filepath.Join(clientOutput, resources.LibraryConfigurationName))
	if err != nil {
		t.Fatalf("read %s: %v", resources.LibraryConfigurationName, err)
	}
	manifest := string(manifestData)
	if !strings.Contains(manifest, "contract-digest: sha256:") {
		t.Fatalf("%s missing contract-digest:\n%s", resources.LibraryConfigurationName, manifest)
	}

	assertLibraryLayout(t, clientOutput, "go", "typescript", "python")
	goVetLibrary(t, filepath.Join(clientOutput, "go"))

	facadeFile := findFacadeFile(t, filepath.Join(clientOutput, "go"), "_facade.pb.go")
	assertFileContains(t, facadeFile, "func New(gw Gateway")

	pyFacade := findFacadeFile(t, filepath.Join(clientOutput, "python", "audit_client"), "api.py")
	assertFileContains(t, pyFacade, "class ApiServiceClient")
	assertPythonImports(t, filepath.Join(clientOutput, "python"), "audit_client.api")

	tsFacade := findFacadeFile(t, filepath.Join(clientOutput, "typescript"), "_facade.ts")
	assertFileContains(t, tsFacade, "export const")
}

func TestGenerateClientIsIdempotent(t *testing.T) {
	requireQualify(t)
	moduleDir := scaffoldGoGRPCFixture(t)
	wsDir := filepath.Dir(moduleDir)

	t.Chdir(wsDir)
	output := filepath.Join(t.TempDir(), "lib")

	run := func() {
		resetClientFlags(t)
		clientFrom = "billing/api/grpc"
		clientLanguages = []string{"go"}
		clientOutput = output
		clientForce = true
		if err := ClientCmd.RunE(ClientCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	}

	run()
	first := hashTree(t, output)
	run()
	second := hashTree(t, output)
	if first != second {
		t.Fatalf("second run changed the output tree: %s != %s", first, second)
	}
}

// TestGenerateClientPreservesHandEditedFiles guards against generateLanguage
// wiping the whole per-language directory: a hand-edited go.mod (a comment —
// an added, genuinely-imported require would be indistinguishable from one
// `go mod tidy` legitimately adds itself, and an added-but-unused one is
// exactly what `go mod tidy`, which runs every regenerate, correctly prunes;
// a comment is untouched by tidy either way and unambiguously proves the
// file was left alone rather than rewritten from the template), a hand-edited
// package.json (an added field `go mod tidy` has no opinion on at all), and
// an unrelated file a developer might reasonably drop next to the facade (a
// README) must all survive a second, idempotent run.
func TestGenerateClientPreservesHandEditedFiles(t *testing.T) {
	requireQualify(t)
	moduleDir := scaffoldGoGRPCFixture(t)
	wsDir := filepath.Dir(moduleDir)

	t.Chdir(wsDir)
	output := filepath.Join(t.TempDir(), "lib")

	resetClientFlags(t)
	clientFrom = "billing/api/grpc"
	clientLanguages = []string{"go", "typescript"}
	clientOutput = output
	if err := ClientCmd.RunE(ClientCmd, nil); err != nil {
		t.Fatalf("first RunE: %v", err)
	}

	goModPath := filepath.Join(output, "go", "go.mod")
	original, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatal(err)
	}
	handEditedGoMod := "// hand-edited: do not regenerate blindly\n" + string(original)
	if err := os.WriteFile(goModPath, []byte(handEditedGoMod), 0o644); err != nil {
		t.Fatal(err)
	}
	readmePath := filepath.Join(output, "go", "README.md")
	if err := os.WriteFile(readmePath, []byte("hand-written notes"), 0o644); err != nil {
		t.Fatal(err)
	}

	packageJSONPath := filepath.Join(output, "typescript", "package.json")
	originalPackageJSON, err := os.ReadFile(packageJSONPath)
	if err != nil {
		t.Fatal(err)
	}
	var packageJSON map[string]any
	if err := json.Unmarshal(originalPackageJSON, &packageJSON); err != nil {
		t.Fatal(err)
	}
	packageJSON["private"] = true
	handEditedPackageJSON, err := json.Marshal(packageJSON)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(packageJSONPath, handEditedPackageJSON, 0o644); err != nil {
		t.Fatal(err)
	}

	resetClientFlags(t)
	clientFrom = "billing/api/grpc"
	clientLanguages = []string{"go", "typescript"}
	clientOutput = output
	if err := ClientCmd.RunE(ClientCmd, nil); err != nil {
		t.Fatalf("second RunE: %v", err)
	}

	afterGoMod, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("go.mod was removed by the second run: %v", err)
	}
	if !strings.HasPrefix(string(afterGoMod), "// hand-edited: do not regenerate blindly\n") {
		t.Fatalf("hand-edited go.mod was overwritten:\ngot:  %q", afterGoMod)
	}
	if _, err := os.Stat(readmePath); err != nil {
		t.Fatalf("hand-added README.md was removed by the second run: %v", err)
	}
	afterPackageJSON, err := os.ReadFile(packageJSONPath)
	if err != nil {
		t.Fatalf("package.json was removed by the second run: %v", err)
	}
	var afterFields map[string]any
	if err := json.Unmarshal(afterPackageJSON, &afterFields); err != nil {
		t.Fatal(err)
	}
	if afterFields["private"] != true {
		t.Fatalf("hand-edited package.json was overwritten: %s", afterPackageJSON)
	}
}

// TestGenerateClientPartialLanguageRerunKeepsManifest guards against
// writeLibraryManifest overwriting languages: wholesale: requesting only a
// subset of previously-generated languages must not drop the others from the
// manifest — their directories are untouched and still valid.
func TestGenerateClientPartialLanguageRerunKeepsManifest(t *testing.T) {
	requireQualify(t)
	moduleDir := scaffoldGoGRPCFixture(t)
	wsDir := filepath.Dir(moduleDir)

	t.Chdir(wsDir)
	output := filepath.Join(t.TempDir(), "lib")

	resetClientFlags(t)
	clientFrom = "billing/api/grpc"
	clientLanguages = []string{"go", "typescript"}
	clientOutput = output
	if err := ClientCmd.RunE(ClientCmd, nil); err != nil {
		t.Fatalf("first RunE: %v", err)
	}

	resetClientFlags(t)
	clientFrom = "billing/api/grpc"
	clientLanguages = []string{"go"}
	clientOutput = output
	clientForce = true
	if err := ClientCmd.RunE(ClientCmd, nil); err != nil {
		t.Fatalf("second RunE: %v", err)
	}

	exports, err := readExistingLanguageExports(output)
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool, len(exports))
	for _, e := range exports {
		names[e.Name] = true
	}
	if !names["go"] || !names["typescript"] {
		t.Fatalf("manifest languages = %+v, want both go and typescript still declared", exports)
	}
	if _, err := os.Stat(filepath.Join(output, "typescript", "package.json")); err != nil {
		t.Fatalf("typescript/ was removed even though it was not requested: %v", err)
	}
}

func TestGenerateClientRefusesDigestChangeWithoutForce(t *testing.T) {
	// Built from the original working directory (inside the module), before
	// any t.Chdir: scaffoldGoGRPCFixture's `go build` needs to run there.
	// The synthetic fixture below is not a parseable descriptor set (it only
	// needs a stable digest for the ungated check that follows), so
	// proceeding past the refusal with --force needs this real one instead.
	var realModuleDir string
	if os.Getenv("CODEFLY_GENERATE_QUALIFY") == "1" {
		realModuleDir = scaffoldGoGRPCFixture(t)
		t.Chdir(realModuleDir)
		resetContractsFlags(t)
		if err := ContractsCmd.RunE(ContractsCmd, []string{"billing"}); err != nil {
			t.Fatalf("generate contracts: %v", err)
		}
	}

	ctx := context.Background()
	root := t.TempDir()
	workspace := &resources.Workspace{Name: "test-workspace", Layout: resources.LayoutKindModules}
	if err := workspace.SaveToDirUnsafe(ctx, root); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	fixtureDir := writeSyntheticContractsFixture(t)

	libDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(libDir, resources.LibraryConfigurationName), []byte("kind: library\nname: x\nversion: 0.1.0\nsource:\n  contract-digest: sha256:0000000000000000000000000000000000000000000000000000000000000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resetClientFlags(t)
	clientFrom = "contracts:" + fixtureDir
	clientLanguages = []string{"go"}
	clientOutput = libDir

	err := ClientCmd.RunE(ClientCmd, nil)
	if err == nil {
		t.Fatal("expected a digest-mismatch error")
	}
	if !strings.Contains(err.Error(), "use --force") {
		t.Fatalf("error = %v, want it to mention --force", err)
	}

	requireQualify(t)
	resetClientFlags(t)
	clientFrom = "contracts:" + filepath.Join(realModuleDir, "contracts", "api")
	clientLanguages = []string{"go"}
	clientOutput = libDir
	clientForce = true
	if err := ClientCmd.RunE(ClientCmd, nil); err != nil {
		t.Fatalf("--force should proceed: %v", err)
	}
}

func TestGenerateClientPackageSourceRequiresPinnedVersion(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	moduleDir := filepath.Join(root, "vendored-x")
	if err := os.MkdirAll(filepath.Join(moduleDir, "contracts", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `kind: module-package
schema: codefly/module-package/v2
id: codefly/x
version: 0.1.0
minimum-codefly-version: ">=0.0.0"
artifact-roots:
  - contracts/api
contracts:
  composition: ">=2.0"
services: []
`
	if err := os.WriteFile(filepath.Join(moduleDir, "module.package.codefly.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	module := &resources.Module{Kind: resources.ModuleKind, Name: "x"}
	module.WithDir(moduleDir)
	if err := module.Save(ctx); err != nil {
		t.Fatal(err)
	}

	pathOverride := moduleDir
	workspace := &resources.Workspace{
		Name:   "test-workspace",
		Layout: resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{
			{Name: "x", PathOverride: &pathOverride},
		},
	}
	wsDir := filepath.Join(root, "ws")
	if err := workspace.SaveToDirUnsafe(ctx, wsDir); err != nil {
		t.Fatal(err)
	}

	t.Chdir(wsDir)
	resetClientFlags(t)
	clientFrom = "package:codefly/x@0.2.0"
	clientLanguages = []string{"go"}
	clientOutput = filepath.Join(t.TempDir(), "lib")

	err := ClientCmd.RunE(ClientCmd, nil)
	if err == nil {
		t.Fatal("expected a version-mismatch error")
	}
	if !strings.Contains(err.Error(), "composes codefly/x@0.1.0, not @0.2.0") {
		t.Fatalf("error = %v, want it to contain %q", err, "composes codefly/x@0.1.0, not @0.2.0")
	}
}

// TestGenerateClientPackageSourcePreservesResolutionError guards against
// resolvePackageSource collapsing a real per-module failure (here, a missing
// module.package.codefly.yaml) into the generic "workspace does not compose
// package" message, which would send a user chasing the wrong cause (e.g. a
// network/auth failure resolving a pinned artifact) down the wrong path.
func TestGenerateClientPackageSourcePreservesResolutionError(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	moduleDir := filepath.Join(root, "vendored-x")
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// No module.package.codefly.yaml written: LoadPackageManifest fails for
	// this ref before it can even be checked against the requested id.

	pathOverride := moduleDir
	workspace := &resources.Workspace{
		Name:   "test-workspace",
		Layout: resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{
			{Name: "x", PathOverride: &pathOverride},
		},
	}
	wsDir := filepath.Join(root, "ws")
	if err := workspace.SaveToDirUnsafe(ctx, wsDir); err != nil {
		t.Fatal(err)
	}

	t.Chdir(wsDir)
	resetClientFlags(t)
	clientFrom = "package:codefly/x@0.1.0"
	clientLanguages = []string{"go"}
	clientOutput = filepath.Join(t.TempDir(), "lib")

	err := ClientCmd.RunE(ClientCmd, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "x: cannot load package manifest") {
		t.Fatalf("error = %v, want it to preserve the per-module failure (%q)", err, "x: cannot load package manifest")
	}
}

// TestGenerateClientLocalSourceRejectsEndpointFlag guards against --endpoint
// silently doing nothing for a local --from: a local source names its
// endpoint as --from's own third segment, and --endpoint was previously
// accepted but never consulted for it.
func TestGenerateClientLocalSourceRejectsEndpointFlag(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	workspace := &resources.Workspace{
		Name:    "test-workspace",
		Layout:  resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{{Name: "billing"}},
	}
	if err := workspace.SaveToDirUnsafe(ctx, root); err != nil {
		t.Fatal(err)
	}
	module := &resources.Module{Kind: resources.ModuleKind, Name: "billing"}
	module.WithDir(filepath.Join(root, "modules", "billing"))
	if err := module.Save(ctx); err != nil {
		t.Fatal(err)
	}

	t.Chdir(root)
	resetClientFlags(t)
	clientFrom = "billing/api"
	clientEndpoint = "grpc"
	clientLanguages = []string{"go"}
	clientOutput = filepath.Join(t.TempDir(), "lib")

	err := ClientCmd.RunE(ClientCmd, nil)
	if err == nil {
		t.Fatal("expected an error rejecting --endpoint for a local --from")
	}
	if !strings.Contains(err.Error(), "--endpoint does not apply") {
		t.Fatalf("error = %v, want it to contain %q", err, "--endpoint does not apply")
	}
}

// The GenerateCmd.Find("grpc") resolution itself is asserted in
// cmd/generate_test.go (package cmd), which owns GenerateCmd; this test
// covers what the resolved alias does when run.
func TestLegacyAliasesStillRun(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	workspace := &resources.Workspace{Name: "test-workspace", Layout: resources.LayoutKindModules}
	if err := workspace.SaveToDirUnsafe(ctx, root); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	serviceInput, languageInput, destination = "does-not-exist", "go", filepath.Join(t.TempDir(), "out")
	t.Cleanup(func() { serviceInput, languageInput, destination = "", "", "" })

	out, _ := captureStderr(t, func() error {
		return GRPCCmd.RunE(GRPCCmd, nil)
	})
	if !strings.Contains(out, "deprecated") {
		t.Fatalf("stderr = %q, want it to contain %q", out, "deprecated")
	}
}

func assertLibraryLayout(t *testing.T, outputDir string, langs ...string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(outputDir, resources.LibraryConfigurationName)); err != nil {
		t.Fatalf("missing %s: %v", resources.LibraryConfigurationName, err)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "contract", "catalog.codefly.json")); err != nil {
		t.Fatalf("missing contract/catalog.codefly.json: %v", err)
	}
	for _, lang := range langs {
		if _, err := os.Stat(filepath.Join(outputDir, lang)); err != nil {
			t.Fatalf("missing %s/: %v", lang, err)
		}
	}
}

func findFacadeFile(t *testing.T, root, suffix string) string {
	t.Helper()
	var found string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, suffix) {
			found = p
		}
		return nil
	})
	return found
}

func assertFileContains(t *testing.T, path, substr string) {
	t.Helper()
	if path == "" {
		t.Fatalf("no file found to check for %q", substr)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(data), substr) {
		t.Fatalf("%s does not contain %q", path, substr)
	}
}

func goVetLibrary(t *testing.T, goDir string) {
	t.Helper()
	cmd := exec.Command("go", "vet", "./...")
	cmd.Dir = goDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go vet %s: %v\n%s", goDir, err, out)
	}
}

// assertPythonImports proves the generated Python export is actually
// importable, not just present: a string-content check alone previously let
// a broken bindings layout ship (the facade's own `import <flat_module>`
// failing with ModuleNotFoundError, since string-matching cannot see whether
// a module resolves). Runs inside the same proto companion image core's own
// facade tests use, so it needs neither a local Python nor the generated
// library's pinned protobuf dependency installed on the host.
func assertPythonImports(t *testing.T, pythonDir, dottedModule string) {
	t.Helper()
	ctx := context.Background()
	image, err := coreproto.CompanionImage(ctx)
	if err != nil {
		t.Fatalf("CompanionImage: %v", err)
	}
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm",
		"-v", pythonDir+":/out", "-w", "/out", image.Name+":"+image.Tag,
		"/venv/bin/python", "-c", "import "+dottedModule)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("import %s failed: %v\n%s", dottedModule, err, out)
	}
}

func hashTree(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	var paths []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		paths = append(paths, rel)
		return nil
	})
	sort.Strings(paths)
	for _, rel := range paths {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		h.Write([]byte(rel))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeSyntheticContractsFixture writes a self-consistent (but not
// buf-parseable) contracts/api directory: enough for the digest-refusal path,
// which never reaches proto.GenerateClient, to exercise without Docker.
func writeSyntheticContractsFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "contracts", "api")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	contractBytes := []byte("synthetic-descriptor-set")
	if err := os.WriteFile(filepath.Join(dir, "contract.binpb"), contractBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	catalog := &composition.APIContractCatalog{
		Schema:  composition.APIContractCatalogSchema,
		Package: "codefly/synthetic",
		Version: "0.1.0",
		Endpoints: []composition.APIContractEndpoint{{
			Service:  "api",
			Endpoint: "grpc",
			API:      "grpc",
			Kind:     composition.APIContractKindProtobuf,
			Package:  "synthetic.v1",
			Path:     "contracts/api/contract.binpb",
			Digest:   composition.APIContractDigest(contractBytes),
			Services: []composition.APIContractService{{Name: "Svc", FullName: "synthetic.v1.Svc", Procedures: []string{"/synthetic.v1.Svc/Do"}}},
		}},
	}
	data, err := catalog.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "catalog.codefly.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}
