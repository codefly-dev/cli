package generate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/cli"
	coreproto "github.com/codefly-dev/core/companions/proto"
	"github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/languages"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/standards"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"gopkg.in/yaml.v3"
)

// Versions pinned to the same values `generate contracts`/`generate grpc` use
// (cmd/generate/proto.go's pinnedProtocPlugins) and the ones core's own
// facade tests vet against (companions/proto/facade_test.go's goVet).
const (
	pinnedConnectVersion  = "v1.20.0"
	pinnedProtobufVersion = "v1.36.11"
	goLanguageVersion     = "1.24"
)

// generatedLanguageExport is one languages: entry of library.codefly.yaml.
type generatedLanguageExport struct {
	Name    string
	Path    string
	Exports []string
}

// generateLanguage generates one language's export for the library: the
// per-language directory, bindings under gen/ (or _gen/ for python), the
// facade hoisted alongside it, and the language's own package scaffolding.
//
// langDir itself is never wiped: it is not a purely generated directory — the
// facade and scaffold files (go.mod, package.json, pyproject.toml,
// tsconfig.json) live at its root by design, exactly where a developer would
// naturally add a README, a license, or a hand-edit to a scaffold file (a
// bumped dependency, a private registry setting). Only the pieces this
// command knows are entirely its own — the gen/ (_gen/ for python)
// subdirectory and the single, by-name-identified facade file — are removed
// and rewritten; scaffold files are written once and never touched again
// once present (see writeFileIfMissing), so a hand-edit survives a rerun,
// including an idempotent one where nothing about the contract changed.
func generateLanguage(ctx context.Context, lang languages.Language, outputDir, moduleName string, servicesFilter []string, facade bool, source *resolvedContractSource, goModule, npmPackage, pyPackage string) (generatedLanguageExport, error) {
	langDir := filepath.Join(outputDir, string(lang))
	if err := os.MkdirAll(langDir, 0o755); err != nil {
		return generatedLanguageExport{}, err
	}

	if source.endpoint.Kind == composition.APIContractKindOpenAPI {
		return generateOpenAPILanguage(ctx, lang, langDir, source, goModule, npmPackage)
	}
	return generateProtobufLanguage(ctx, lang, langDir, moduleName, servicesFilter, facade, source, goModule, npmPackage, pyPackage)
}

// generateProtobufLanguage runs proto.GenerateClient into a scratch
// directory, then reshapes its output into the library layout: generated
// bindings under gen/ (python: <pkg>/_gen/), the generated facade hoisted
// alongside it, and the language's own package scaffolding.
func generateProtobufLanguage(ctx context.Context, lang languages.Language, langDir, moduleName string, servicesFilter []string, facade bool, source *resolvedContractSource, goModule, npmPackage, pyPackage string) (generatedLanguageExport, error) {
	raw, err := os.MkdirTemp("", "codefly-client-raw")
	if err != nil {
		return generatedLanguageExport{}, err
	}
	defer func() { _ = os.RemoveAll(raw) }()

	req := coreproto.ClientRequest{
		Language:    lang,
		Destination: raw,
		Module:      moduleName,
		Services:    servicesFilter,
		Facade:      facade,
	}
	if len(source.protoSources) > 0 {
		// See resolvedContractSource.protoSources: generating from the
		// original files instead of the descriptor set sidesteps a core bug
		// where a --services subset over a multi-file descriptor set reports
		// the target service as undeclared.
		req.Sources = source.protoSources
	} else {
		var set descriptorpb.FileDescriptorSet
		if err := googleproto.Unmarshal(source.contractBytes, &set); err != nil {
			return generatedLanguageExport{}, fmt.Errorf("cannot parse contract descriptor set: %w", err)
		}
		req.DescriptorSet = source.contractBytes
		req.TargetFiles = targetFilesForPackage(&set, source.endpoint.Package)
	}

	if err := coreproto.GenerateClient(ctx, req); err != nil {
		return generatedLanguageExport{}, err
	}

	switch lang {
	case languages.GO:
		return assembleGoLibrary(langDir, raw, moduleName, goModule)
	case languages.TYPESCRIPT:
		export, err := assembleTypeScriptLibrary(langDir, raw, npmPackage)
		if err != nil {
			return export, err
		}
		return export, writeTypeScriptScaffold(langDir, npmPackage)
	case languages.PYTHON:
		return assemblePythonLibrary(langDir, raw, moduleName, pyPackage)
	default:
		return generatedLanguageExport{}, fmt.Errorf("language %s is not supported for client generation", lang)
	}
}

// generateOpenAPILanguage synthesizes a basev0.Endpoint carrying the raw
// OpenAPI document (the only thing proto.GenerateOpenAPI reads) and runs it.
// OpenAPI clients have no facade: proto.GenerateOpenAPI's contract-derived
// codegen is a plain typed HTTP client.
func generateOpenAPILanguage(ctx context.Context, lang languages.Language, langDir string, source *resolvedContractSource, goModule, npmPackage string) (generatedLanguageExport, error) {
	if lang != languages.GO && lang != languages.TYPESCRIPT {
		return generatedLanguageExport{}, fmt.Errorf("openapi clients support go or typescript, not %s", lang)
	}

	genDir := filepath.Join(langDir, "gen")
	if err := os.RemoveAll(genDir); err != nil {
		return generatedLanguageExport{}, err
	}
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		return generatedLanguageExport{}, err
	}

	endpoint := &basev0.Endpoint{
		Name:    source.endpoint.Endpoint,
		Service: source.endpoint.Service,
		Module:  defaultFacadeModule,
		Api:     standards.REST,
		ApiDetails: &basev0.API{
			Value: &basev0.API_Rest{Rest: &basev0.RestAPI{Openapi: source.contractBytes}},
		},
	}

	if lang == languages.GO {
		// go.mod lives at langDir, not genDir: genDir is fully regenerated
		// every run (it is proto.GenerateOpenAPI's own output), but go.mod is
		// a scaffold file a developer may hand-edit (an added dependency, a
		// replace directive) and findModRoot walks up from genDir to find it
		// either way, so placing it one level up costs nothing and makes it
		// write-once like the protobuf path's go.mod.
		if err := writeGoModule(langDir, goModule); err != nil {
			return generatedLanguageExport{}, err
		}
	}
	if err := coreproto.GenerateOpenAPI(ctx, lang, genDir, "", endpoint); err != nil {
		return generatedLanguageExport{}, err
	}

	if lang == languages.GO {
		bestEffortGoModTidy(langDir)
		return generatedLanguageExport{Name: "go", Path: "go/", Exports: []string{goModule}}, nil
	}
	if err := writeTypeScriptScaffold(langDir, npmPackage); err != nil {
		return generatedLanguageExport{}, err
	}
	return generatedLanguageExport{Name: "typescript", Path: "typescript/", Exports: []string{npmPackage}}, nil
}

// targetFilesForPackage returns the names of the files in set whose proto
// package is pkg — the module's own files, as opposed to shared imports —
// which proto.GenerateClient's TargetFiles needs for a Python facade.
func targetFilesForPackage(set *descriptorpb.FileDescriptorSet, pkg string) []string {
	var files []string
	for _, f := range set.GetFile() {
		if f.GetPackage() == pkg {
			files = append(files, f.GetName())
		}
	}
	sort.Strings(files)
	return files
}

// goFacadeImportRe matches a single Go import line ("ident? \"path\"") whose
// path starts with oldPrefix — the shape buf's generated .go files use for
// both the import block and single-import statements. It is anchored to the
// whole (trimmed of trailing whitespace) line so it cannot match the same
// text appearing inside an unrelated multi-field literal, such as the raw
// FileDescriptorProto bytes embedded in *.pb.go for reflection: those are
// never alone on their own line in the shape "ident \"path\"".
func goImportLineRe(oldPrefix string) *regexp.Regexp {
	return regexp.MustCompile(`^(\s*(?:[A-Za-z_][A-Za-z0-9_]*\s+)?)"` + regexp.QuoteMeta(oldPrefix) + `((?:/[A-Za-z0-9_./-]*)?)"(\s*)$`)
}

// rewriteGoImportPrefix rewrites every Go import line under root whose path
// starts with oldPrefix to start with newPrefix instead, leaving every other
// byte of every file untouched.
//
// proto.GenerateClient's Go facade plugin hardcodes the generated code's
// import path to github.com/codefly-dev/cli/pkg/builder/clients/<module>
// (core's CreateBufConfiguration is not parameterized for a standalone
// destination module), so the facade file and the connect stubs cross-import
// the bindings package via that literal path regardless of where the caller
// asked for the files to be written. Left alone, a library's own go.mod
// (whose module path is its real publish name) would not satisfy those
// imports, and `go build` would try to fetch github.com/codefly-dev/cli — a
// real, different module — from the network to resolve them.
func rewriteGoImportPrefix(root, oldPrefix, newPrefix string) error {
	re := goImportLineRe(oldPrefix)
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		data, err := os.ReadFile(p) //nolint:gosec // p is walked from a scratch dir this command owns
		if err != nil {
			return err
		}
		lines := strings.Split(string(data), "\n")
		changed := false
		for i, line := range lines {
			if m := re.FindStringSubmatch(line); m != nil {
				lines[i] = m[1] + `"` + newPrefix + m[2] + `"` + m[3]
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o644) //nolint:gosec
	})
}

// assembleGoLibrary rewrites raw's hardcoded import prefix to goModule/gen,
// moves the *_facade.pb.go file(s) to the go/ root, everything else to
// go/gen/, and writes go.mod.
func assembleGoLibrary(langDir, raw, moduleName, goModule string) (generatedLanguageExport, error) {
	service := moduleName
	if service == "" {
		service = defaultFacadeModule
	}
	oldPrefix := "github.com/codefly-dev/cli/pkg/builder/clients/" + service
	if err := rewriteGoImportPrefix(raw, oldPrefix, goModule+"/gen"); err != nil {
		return generatedLanguageExport{}, fmt.Errorf("cannot rewrite generated import paths: %w", err)
	}

	genDir := filepath.Join(langDir, "gen")
	if err := os.RemoveAll(genDir); err != nil {
		return generatedLanguageExport{}, err
	}
	// A prior run's facade file(s), if --module-name changed since (a
	// different basename), would otherwise linger at langDir's root
	// alongside the new one: langDir itself is no longer wiped wholesale
	// (see generateLanguage), so any generated-facade leftover needs its own
	// explicit cleanup, scoped by the one filename shape this command ever
	// writes there.
	if err := removeMatching(langDir, "*_facade.pb.go"); err != nil {
		return generatedLanguageExport{}, err
	}
	if err := moveTreeSplitBySuffix(raw, genDir, langDir, "_facade.pb.go"); err != nil {
		return generatedLanguageExport{}, err
	}
	if err := writeGoModule(langDir, goModule); err != nil {
		return generatedLanguageExport{}, err
	}
	bestEffortGoModTidy(langDir)
	return generatedLanguageExport{Name: "go", Path: "go/", Exports: []string{goModule}}, nil
}

// assembleTypeScriptLibrary moves raw's tree under typescript/src/gen/, hoists
// the *_facade.ts file(s) to typescript/src/, and rewrites the facade's
// same-directory relative import ("./x") to reach into its new location under
// gen/ instead.
func assembleTypeScriptLibrary(langDir, raw, npmPackage string) (generatedLanguageExport, error) {
	srcDir := filepath.Join(langDir, "src")
	// Unlike go/ and python/, the scaffold files here (package.json,
	// tsconfig.json) live at langDir's root, not under src/: src/ in its
	// entirety is this command's own output, so it is safe (and simplest) to
	// wipe the whole subtree rather than track a facade filename that can
	// change across runs.
	if err := os.RemoveAll(srcDir); err != nil {
		return generatedLanguageExport{}, err
	}
	genDir := filepath.Join(srcDir, "gen")
	facadeFiles, err := moveTreeSplitBySuffixWithRelDir(raw, genDir, srcDir, "_facade.ts")
	if err != nil {
		return generatedLanguageExport{}, err
	}
	relImportRe := regexp.MustCompile(`(from\s+")\.\/`)
	for facadePath, relDir := range facadeFiles {
		data, err := os.ReadFile(facadePath) //nolint:gosec // facadePath is a file this command just wrote
		if err != nil {
			return generatedLanguageExport{}, err
		}
		rewritten := relImportRe.ReplaceAll(data, []byte(`${1}./gen/`+filepath.ToSlash(relDir)+`/`))
		if err := os.WriteFile(facadePath, rewritten, 0o644); err != nil { //nolint:gosec
			return generatedLanguageExport{}, err
		}
	}
	return generatedLanguageExport{Name: "typescript", Path: "typescript/", Exports: []string{npmPackage}}, nil
}

// assemblePythonLibrary moves raw's bindings under pyPackage/_gen/, hoists
// the facade file (proto.GenerateClient's Python facade plugin always writes
// it at destination root, named "<moduleName>.py") to pyPackage/, and
// rewrites both the facade's and the bindings' own import references to go
// through pyPackage._gen instead.
func assemblePythonLibrary(langDir, raw, moduleName, pyPackage string) (generatedLanguageExport, error) {
	pkgDir := filepath.Join(langDir, pyPackage)
	genDir := filepath.Join(pkgDir, "_gen")
	facadeFileName := moduleName + ".py"
	if err := os.RemoveAll(genDir); err != nil {
		return generatedLanguageExport{}, err
	}
	// A prior run's facade, if --module-name changed since (a different
	// basename), would otherwise linger at pkgDir's root: nothing else this
	// command writes there matches "*.py" other than __init__.py, so this
	// only ever removes a stale generated facade, never a hand-added module.
	if err := removeMatching(pkgDir, "*.py", "__init__.py"); err != nil {
		return generatedLanguageExport{}, err
	}
	if err := movePythonTree(raw, genDir, pkgDir, facadeFileName); err != nil {
		return generatedLanguageExport{}, err
	}
	if err := writePythonInitFiles(genDir); err != nil {
		return generatedLanguageExport{}, err
	}
	if err := writeFileIfMissing(filepath.Join(pkgDir, "__init__.py"), nil); err != nil {
		return generatedLanguageExport{}, err
	}
	if err := rewritePythonImportPrefix(pkgDir, findPythonImportRoots(raw, facadeFileName), pyPackage+"._gen"); err != nil {
		return generatedLanguageExport{}, err
	}
	if err := writePythonScaffold(langDir, pyPackage); err != nil {
		return generatedLanguageExport{}, err
	}
	return generatedLanguageExport{Name: "python", Path: "python/", Exports: []string{pyPackage}}, nil
}

// movePythonTree moves raw's tree into dst (preserving each file's relative
// path, whatever its depth) except the exact facade file, which moves to
// flatDst. Depth cannot distinguish "the facade" from "a binding": a proto
// file with no package-derived directory of its own (e.g. a service scaffold
// whose .proto declares no versioned package, so its *_pb2.py lands at raw's
// root too) previously made every top-level file look like the facade,
// leaving bindings never moved into _gen/ and the facade's own `import
// <binding>` (now a sibling, not a package member) unresolvable at runtime —
// confirmed as ModuleNotFoundError, not just a layout nit.
func movePythonTree(src, dst, flatDst, facadeFileName string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == facadeFileName {
			return copyFile(p, filepath.Join(flatDst, rel))
		}
		return copyFile(p, filepath.Join(dst, rel))
	})
}

// moveTreeSplitBySuffix moves every file under src into dst (preserving
// relative paths) except files ending in suffix, which move directly into
// flatDst instead (flattened, basename only).
func moveTreeSplitBySuffix(src, dst, flatDst, suffix string) error {
	_, err := moveTreeSplitBySuffixWithRelDir(src, dst, flatDst, suffix)
	return err
}

// moveTreeSplitBySuffixWithRelDir is moveTreeSplitBySuffix, additionally
// returning each hoisted file's original directory relative to src (so a
// caller can fix up a relative import that assumed a sibling file).
func moveTreeSplitBySuffixWithRelDir(src, dst, flatDst, suffix string) (map[string]string, error) {
	hoisted := map[string]string{}
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if strings.HasSuffix(p, suffix) {
			target := filepath.Join(flatDst, filepath.Base(p))
			if err := copyFile(p, target); err != nil {
				return err
			}
			hoisted[target] = filepath.ToSlash(filepath.Dir(rel))
			return nil
		}
		return copyFile(p, filepath.Join(dst, rel))
	})
	return hoisted, err
}

// findPythonImportRoots returns the import roots the facade/bindings
// generated in raw actually use: for a binding under a proto-package
// directory (e.g. "saas/accounts/v1/audit_pb2.py"), the dotted directory
// ("saas.accounts.v1"); for a binding with no directory component of its own
// (a proto file with no package-derived subdirectory, e.g. "api_pb2.py" at
// raw's root), its own module name ("api_pb2") — the flat `import api_pb2`
// form the facade uses in that case. facadeFileName is excluded: nothing
// imports the facade module by name. Used to scope the import-prefix
// rewrite to exactly the modules this generation produced, not any
// substring match.
func findPythonImportRoots(raw, facadeFileName string) []string {
	seen := map[string]bool{}
	var roots []string
	_ = filepath.WalkDir(raw, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".py") {
			return nil
		}
		rel, err := filepath.Rel(raw, p)
		if err != nil || rel == facadeFileName {
			return nil
		}
		var root string
		if dir := filepath.Dir(rel); dir == "." {
			root = strings.TrimSuffix(filepath.Base(rel), ".py")
		} else {
			root = strings.ReplaceAll(filepath.ToSlash(dir), "/", ".")
		}
		if !seen[root] {
			seen[root] = true
			roots = append(roots, root)
		}
		return nil
	})
	sort.Strings(roots)
	return roots
}

// pythonImportLineRe matches a single "import <pkg>..." or "from <pkg> ..."
// line whose dotted path starts with pkg, anchored to the start of the line
// (after leading whitespace) so it only ever touches genuine import
// statements.
func pythonImportLineRe(pkg string) *regexp.Regexp {
	escaped := regexp.QuoteMeta(pkg)
	return regexp.MustCompile(`^(\s*(?:import|from)\s+)` + escaped + `(\.[A-Za-z0-9_.]*)?(\b.*)$`)
}

// rewritePythonImportPrefix rewrites every "import <pkg>..."/"from <pkg> ..."
// line under root, for every pkg in pkgs, to use newPrefix+"."+pkg instead —
// the bindings moved from being importable at their proto-package path to
// living under pyPackage._gen.
func rewritePythonImportPrefix(root string, pkgs []string, newPrefix string) error {
	matchers := make([]*regexp.Regexp, 0, len(pkgs))
	for _, pkg := range pkgs {
		matchers = append(matchers, pythonImportLineRe(pkg))
	}
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".py") {
			return err
		}
		data, err := os.ReadFile(p) //nolint:gosec // p is walked from a scratch/output dir this command owns
		if err != nil {
			return err
		}
		lines := strings.Split(string(data), "\n")
		changed := false
		for i, line := range lines {
			for j, pkg := range pkgs {
				if m := matchers[j].FindStringSubmatch(line); m != nil {
					lines[i] = m[1] + newPrefix + "." + pkg + m[2] + m[3]
					changed = true
					break
				}
			}
		}
		if !changed {
			return nil
		}
		return os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o644) //nolint:gosec
	})
}

func writePythonInitFiles(genDir string) error {
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(genDir, "__init__.py"), nil, 0o644); err != nil { //nolint:gosec
		return err
	}
	return filepath.WalkDir(genDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		return os.WriteFile(filepath.Join(p, "__init__.py"), nil, 0o644) //nolint:gosec
	})
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src) //nolint:gosec // src is walked from a scratch dir this command owns
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst) //nolint:gosec // dst is under this command's own output directory
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// removeMatching removes every file directly in dir (non-recursive) whose
// basename matches pattern, except any name listed in keep. Used to clear a
// stale generated artifact (e.g. a facade file left over from a
// --module-name that has since changed) before writing its replacement,
// without touching anything else in a directory this command does not fully
// own.
func removeMatching(dir, pattern string, keep ...string) error {
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return err
	}
	kept := make(map[string]bool, len(keep))
	for _, k := range keep {
		kept[filepath.Join(dir, k)] = true
	}
	for _, m := range matches {
		if kept[m] {
			continue
		}
		if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// writeFileIfMissing writes content to path only if no file is already
// there, so a hand-edited scaffold file (an added dependency, a private
// registry setting, license text) survives a later regenerate instead of
// being silently reset to the template on every run — including a run where
// the contract did not change at all.
func writeFileIfMissing(path string, content []byte) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, content, 0o644) //nolint:gosec
}

func writeGoModule(dir, goModule string) error {
	content := fmt.Sprintf(`module %s

go %s

require (
	connectrpc.com/connect %s
	google.golang.org/protobuf %s
)
`, goModule, goLanguageVersion, pinnedConnectVersion, pinnedProtobufVersion)
	return writeFileIfMissing(filepath.Join(dir, "go.mod"), []byte(content))
}

// bestEffortGoModTidy runs `go mod tidy` if go is on PATH, so the checked-in
// go.mod carries a resolved go.sum. It warns rather than fails: a codegen
// host without Go installed can still produce a usable library.
func bestEffortGoModTidy(dir string) {
	binary, err := exec.LookPath("go")
	if err != nil {
		cli.Warning("go is not on PATH; skipping `go mod tidy` for %s (run it yourself before publishing)", dir)
		return
	}
	cmd := exec.Command(binary, "mod", "tidy")
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		cli.Warning("go mod tidy failed for %s: %v\n%s", dir, err, out.String())
	}
}

func writeTypeScriptScaffold(langDir, npmPackage string) error {
	packageJSON := fmt.Sprintf(`{
  "name": %q,
  "version": "0.0.1",
  "type": "module",
  "exports": {
    ".": "./src/index.ts"
  },
  "files": [
    "dist",
    "README.md"
  ],
  "dependencies": {
    "@bufbuild/protobuf": "^2.0.0",
    "@connectrpc/connect": "^2.0.0"
  },
  "publishConfig": {
    "registry": "https://npm.pkg.github.com"
  }
}
`, npmPackage)
	if err := writeFileIfMissing(filepath.Join(langDir, "package.json"), []byte(packageJSON)); err != nil {
		return err
	}
	tsconfig := `{
  "compilerOptions": {
    "target": "es2022",
    "module": "esnext",
    "moduleResolution": "bundler",
    "strict": true,
    "declaration": true,
    "outDir": "dist",
    "rootDir": "src",
    "skipLibCheck": true
  },
  "include": ["src"]
}
`
	return writeFileIfMissing(filepath.Join(langDir, "tsconfig.json"), []byte(tsconfig))
}

func writePythonScaffold(langDir, pyPackage string) error {
	pyproject := fmt.Sprintf(`[project]
name = %q
version = "0.0.1"
dependencies = ["protobuf>=5.29.3,<7"]

[build-system]
requires = ["setuptools>=68"]
build-backend = "setuptools.build_meta"

[tool.setuptools.packages.find]
include = [%q, %q]
`, strings.ReplaceAll(pyPackage, "_", "-"), pyPackage, pyPackage+".*")
	return writeFileIfMissing(filepath.Join(langDir, "pyproject.toml"), []byte(pyproject))
}

// writeLibraryManifest writes library.codefly.yaml. It is written directly
// (not through resources.Library, whose fields are a subset of this schema)
// so the source: and generated-by: provenance survives; resources.Library
// still loads the file fine, since a plain YAML decode ignores fields it does
// not declare.
func writeLibraryManifest(ctx context.Context, outputDir, name string, source *resolvedContractSource, langExports []generatedLanguageExport, facade bool) error {
	type languageEntry struct {
		Name    string   `yaml:"name"`
		Path    string   `yaml:"path"`
		Exports []string `yaml:"exports"`
	}
	type sourceEntry struct {
		Package        string   `yaml:"package"`
		Version        string   `yaml:"version"`
		Module         string   `yaml:"module,omitempty"`
		Service        string   `yaml:"service"`
		Endpoint       string   `yaml:"endpoint"`
		ContractDigest string   `yaml:"contract-digest"`
		Services       []string `yaml:"services,omitempty"`
	}
	type generatedByEntry struct {
		Codefly   string `yaml:"codefly"`
		Companion string `yaml:"companion,omitempty"`
		Facade    bool   `yaml:"facade"`
	}
	type manifest struct {
		Kind        string           `yaml:"kind"`
		Name        string           `yaml:"name"`
		Version     string           `yaml:"version"`
		Languages   []languageEntry  `yaml:"languages"`
		Source      sourceEntry      `yaml:"source"`
		GeneratedBy generatedByEntry `yaml:"generated-by"`
	}

	languageEntries := make([]languageEntry, 0, len(langExports))
	for _, export := range langExports {
		languageEntries = append(languageEntries, languageEntry(export))
	}

	codeflyVersion, _ := cli.GetCurrentVersion()
	companion := ""
	if image, err := coreproto.CompanionImage(ctx); err == nil {
		companion = image.Name + ":" + image.Tag
	}

	doc := manifest{
		Kind:      "library",
		Name:      name,
		Version:   source.packageVersion,
		Languages: languageEntries,
		Source: sourceEntry{
			Package:        source.packageID,
			Version:        source.packageVersion,
			Module:         source.moduleName,
			Service:        source.endpoint.Service,
			Endpoint:       source.endpoint.Endpoint,
			ContractDigest: source.endpoint.Digest,
			Services:       protobufServiceNames(source.endpoint.Services),
		},
		GeneratedBy: generatedByEntry{
			Codefly:   codeflyVersion,
			Companion: companion,
			Facade:    facade && source.endpoint.Kind == composition.APIContractKindProtobuf,
		},
	}

	data, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return shared.WriteFileAtomic(ctx, filepath.Join(outputDir, resources.LibraryConfigurationName), data, 0o644)
}

func protobufServiceNames(services []composition.APIContractService) []string {
	if len(services) == 0 {
		return nil
	}
	names := make([]string, len(services))
	for i, s := range services {
		names[i] = s.Name
	}
	return names
}
