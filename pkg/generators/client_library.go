package generators

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/cli"
	coreproto "github.com/codefly-dev/core/companions/proto"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/standards"
)

// Versions pinned to the same values `generate contracts`/`generate grpc` use
// (cmd/generate/proto.go's pinnedProtocPlugins) and the ones core's own
// facade tests vet against (companions/proto/facade_test.go's goVet).
const (
	pinnedConnectVersion  = "v1.20.0"
	pinnedProtobufVersion = "v1.36.11"
	goLanguageVersion     = "1.24"
	// pinnedTypeScriptVersion builds the generated library's dist/. The
	// scaffold declares it so `pnpm build` works in a fresh checkout of the
	// library without the consumer supplying a compiler.
	pinnedTypeScriptVersion = "^5.9.3"
)

// langDir itself is never wiped by any of the wipe*/merge*/finalize*
// functions below: it is not a purely generated directory — the facade and
// scaffold files (go.mod, package.json, pyproject.toml, tsconfig.json) live
// at its root by design, exactly where a developer would naturally add a
// README, a license, or a hand-edit to a scaffold file (a bumped dependency,
// a private registry setting). Only the pieces this command knows are
// entirely its own — the gen/ (_gen/ for python) subdirectory and the
// by-name-identified facade file(s) — are removed and rewritten; scaffold
// files are written once and never touched again once present (see
// writeFileIfMissing), so a hand-edit survives a rerun, including an
// idempotent one where nothing about the contract changed.

// wipeGoLibrary clears langDir's generated content once, before any entry is
// merged in.
func wipeGoLibrary(langDir string) error {
	if err := os.RemoveAll(filepath.Join(langDir, "gen")); err != nil {
		return err
	}
	if err := removeMatching(langDir, "*_facade.pb.go"); err != nil {
		return err
	}
	// A facade package (FacadePackages) is go/<module>/: its facade file is
	// this command's, the directory only while nothing else lives in it.
	packages, err := filepath.Glob(filepath.Join(langDir, "*", "*_facade.pb.go"))
	if err != nil {
		return err
	}
	for _, facade := range packages {
		if err := os.Remove(facade); err != nil && !os.IsNotExist(err) {
			return err
		}
		_ = os.Remove(filepath.Dir(facade)) // kept when a hand-added file remains
	}
	return nil
}

// mergeGoEntry rewrites raw's hardcoded import prefix to goModule/gen (keyed
// to this entry's own facade module name) and moves it into langDir: the
// *_facade.pb.go file(s) to the go/ root, everything else to go/gen/. With
// facadePackages the entry's facade moves to its own package go/<module>/ and
// its bindings to go/gen/<module>/: buf writes a binding where its proto file
// sits in the contract, and two contracts each rooted at an api.proto would
// otherwise write one go/gen/api.pb.go.
func mergeGoEntry(langDir, raw, moduleName, goModule string, facadePackages bool) error {
	service := moduleName
	if service == "" {
		service = defaultFacadeModule
	}
	genDir, genImport, facadeDir := filepath.Join(langDir, "gen"), goModule+"/gen", langDir
	if facadePackages {
		genDir, genImport, facadeDir = filepath.Join(genDir, service), genImport+"/"+service, filepath.Join(langDir, service)
	}
	oldPrefix := "github.com/codefly-dev/cli/pkg/builder/clients/" + service
	if err := rewriteGoImportPrefix(raw, oldPrefix, genImport); err != nil {
		return fmt.Errorf("cannot rewrite generated import paths: %w", err)
	}
	return moveTreeSplitBySuffix(raw, genDir, facadeDir, "_facade.pb.go")
}

// finalizeGoLibrary writes go.mod (if absent), resolves it and proves the
// library compiles, once every entry has been merged in.
func finalizeGoLibrary(langDir, goModule string) (generatedLanguageExport, error) {
	if err := writeGoModule(langDir, goModule); err != nil {
		return generatedLanguageExport{}, err
	}
	if err := resolveAndVerifyGoLibrary(langDir); err != nil {
		return generatedLanguageExport{}, err
	}
	return generatedLanguageExport{Name: "go", Path: "go/", Exports: []string{goModule}}, nil
}

// wipeTypeScriptLibrary clears langDir's src/ wholesale: unlike go/ and
// python/, TypeScript's scaffold files (package.json, tsconfig.json) live at
// langDir's root, not under src/, so src/ in its entirety is this command's
// own output and safe to wipe rather than track per-entry facade filenames
// that can change across runs.
func wipeTypeScriptLibrary(langDir string) error {
	return os.RemoveAll(filepath.Join(langDir, "src"))
}

// mergeTypeScriptEntry moves raw's tree under src/gen/, hoists the
// *_facade.ts file(s) to src/ — or, with facadePackages, moves the bindings
// under src/gen/<module>/ and the facade to the entry's own src/<module>/,
// beside an index.ts re-exporting that facade and the entry's bindings — and
// rewrites the facade's same-directory relative import ("./x") to reach into
// its new location under gen/ instead.
func mergeTypeScriptEntry(langDir, raw, moduleName string, facadePackages bool) error {
	srcDir := filepath.Join(langDir, "src")
	genDir := filepath.Join(srcDir, "gen")
	facadeDir, toGen := srcDir, "./gen"
	if facadePackages {
		genDir, facadeDir, toGen = filepath.Join(genDir, moduleName), filepath.Join(srcDir, moduleName), "../gen/"+moduleName
	}
	bindings, err := typeScriptModules(raw, "_facade.ts")
	if err != nil {
		return err
	}
	facadeFiles, err := moveTreeSplitBySuffixWithRelDir(raw, genDir, facadeDir, "_facade.ts")
	if err != nil {
		return err
	}
	relImportRe := regexp.MustCompile(`(from\s+")\.\/`)
	for facadePath, relDir := range facadeFiles {
		data, readErr := os.ReadFile(facadePath) //nolint:gosec // facadePath is a file this command just wrote
		if readErr != nil {
			return readErr
		}
		rewritten := relImportRe.ReplaceAll(data, []byte(`${1}`+toGen+`/`+filepath.ToSlash(relDir)+`/`))
		if writeErr := os.WriteFile(facadePath, rewritten, 0o644); writeErr != nil { //nolint:gosec
			return writeErr
		}
	}
	if !facadePackages {
		return nil
	}
	facades, err := typeScriptModules(facadeDir, "")
	if err != nil {
		return err
	}
	modules := make([]string, 0, len(facades)+len(bindings))
	for _, facade := range facades {
		modules = append(modules, "./"+facade)
	}
	for _, binding := range bindings {
		modules = append(modules, toGen+"/"+binding)
	}
	return writeTypeScriptBarrel(filepath.Join(facadeDir, "index.ts"), modules)
}

// typeScriptModules lists the .ts files under dir as the ".js" module
// specifiers the generated sources import each other by, relative to dir and
// sorted, leaving out index.ts and, when excludeSuffix is set, the files
// ending in it.
func typeScriptModules(dir, excludeSuffix string) ([]string, error) {
	var modules []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".ts") || name == "index.ts" || (excludeSuffix != "" && strings.HasSuffix(name, excludeSuffix)) {
			return nil
		}
		relative, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		// The ".js" specifier the generated bindings and facades already use:
		// the sources are ESM and resolve by emitted path, not by source name.
		modules = append(modules, strings.TrimSuffix(filepath.ToSlash(relative), ".ts")+".js")
		return nil
	})
	sort.Strings(modules)
	return modules, err
}

func finalizeTypeScriptLibrary(langDir, npmPackage string, facadePackages bool) (generatedLanguageExport, error) {
	if err := writeTypeScriptScaffold(langDir, npmPackage, facadePackages); err != nil {
		return generatedLanguageExport{}, err
	}
	if err := writeTypeScriptIndex(langDir, facadePackages); err != nil {
		return generatedLanguageExport{}, err
	}
	return generatedLanguageExport{Name: "typescript", Path: "typescript/", Exports: []string{npmPackage}}, nil
}

// writeTypeScriptIndex writes src/index.ts: the barrel re-exporting every
// hoisted facade and every generated binding — or, with facadePackages, every
// facade alone, since two contracts' bindings may declare one name and each
// facade package (src/<module>/index.ts) re-exports its own — and so the one
// file package.json's "." export names. It is generated output, not a
// scaffold — src/ is wholly owned by this command (wipeTypeScriptLibrary
// removes it) and which files exist depends on the contracts generated — so it
// is rewritten on every run instead of written once. Without it the library
// declares an entry point nothing produced and cannot be resolved at all.
func writeTypeScriptIndex(langDir string, facadePackages bool) error {
	srcDir := filepath.Join(langDir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return err
	}
	modules, err := typeScriptModules(srcDir, "")
	if err != nil {
		return err
	}
	exported := make([]string, 0, len(modules))
	for _, module := range modules {
		if facadePackages && !strings.HasSuffix(module, "_facade.js") {
			continue
		}
		exported = append(exported, "./"+module)
	}
	return writeTypeScriptBarrel(filepath.Join(srcDir, "index.ts"), exported)
}

func writeTypeScriptBarrel(path string, modules []string) error {
	var content strings.Builder
	content.WriteString("// generated by codefly generate client; do not edit\n")
	for _, module := range modules {
		fmt.Fprintf(&content, "export * from %q;\n", module)
	}
	return os.WriteFile(path, []byte(content.String()), 0o644) //nolint:gosec
}

// wipePythonLibrary clears pkgDir's generated content once, before any entry
// is merged in.
func wipePythonLibrary(pkgDir string) error {
	if err := os.RemoveAll(filepath.Join(pkgDir, "_gen")); err != nil {
		return err
	}
	return removeMatching(pkgDir, "*.py", "__init__.py")
}

// mergePythonEntry moves raw's bindings under pkgDir/_gen/, hoists the facade
// file (proto.GenerateClient's Python facade plugin always writes it at
// destination root, named "<moduleName>.py") to pkgDir/, and rewrites both
// the facade's and the bindings' own import references to go through
// pyPackage._gen instead.
func mergePythonEntry(pkgDir, raw, moduleName, pyPackage string) error {
	genDir := filepath.Join(pkgDir, "_gen")
	facadeFileName := moduleName + ".py"
	if err := movePythonTree(raw, genDir, pkgDir, facadeFileName); err != nil {
		return err
	}
	return rewritePythonImportPrefix(pkgDir, findPythonImportRoots(raw, facadeFileName), pyPackage+"._gen")
}

func finalizePythonLibrary(langDir, pyPackage string, upstreamPackages []string) (generatedLanguageExport, error) {
	pkgDir := filepath.Join(langDir, pyPackage)
	genDir := filepath.Join(pkgDir, "_gen")
	if err := writePythonInitFiles(genDir); err != nil {
		return generatedLanguageExport{}, err
	}
	if err := writeFileIfMissing(filepath.Join(pkgDir, "__init__.py"), nil); err != nil {
		return generatedLanguageExport{}, err
	}
	if err := writePythonScaffold(langDir, pyPackage); err != nil {
		return generatedLanguageExport{}, err
	}
	if err := ensurePythonDependencies(langDir, upstreamPackages); err != nil {
		return generatedLanguageExport{}, err
	}
	return generatedLanguageExport{Name: "python", Path: "python/", Exports: []string{pyPackage}}, nil
}

// pyprojectDependenciesRe matches pyproject.toml's single-line project
// dependencies array, the shape writePythonScaffold writes.
var pyprojectDependenciesRe = regexp.MustCompile(`(?m)^dependencies = \[([^\]]*)\]$`)

// ensurePythonDependencies adds any missing package to pyproject.toml's
// dependencies, in place.
//
// The scaffold is written once and never overwritten, so a library first
// generated before its bindings stopped vendoring a shared proto module would
// otherwise keep a dependency list that no longer covers its own imports: the
// bindings import `buf.validate.validate_pb2` absolutely, expecting the
// upstream distribution to supply it. Editing the array rather than rewriting
// the file keeps every hand-edit around it intact.
func ensurePythonDependencies(langDir string, packages []string) error {
	if len(packages) == 0 {
		return nil
	}
	path := filepath.Join(langDir, "pyproject.toml")
	data, err := os.ReadFile(path) //nolint:gosec // path is a file this command just wrote
	if err != nil {
		return err
	}
	match := pyprojectDependenciesRe.FindSubmatchIndex(data)
	if match == nil {
		return fmt.Errorf("cannot find a dependencies array in %s", path)
	}
	existing := string(data[match[2]:match[3]])
	var missing []string
	for _, pkg := range packages {
		if !strings.Contains(existing, `"`+pkg+`"`) {
			missing = append(missing, `"`+pkg+`"`)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	added := existing
	if strings.TrimSpace(added) != "" {
		added += ", "
	}
	added += strings.Join(missing, ", ")
	updated := append([]byte{}, data[:match[2]]...)
	updated = append(updated, added...)
	updated = append(updated, data[match[3]:]...)
	return os.WriteFile(path, updated, 0o644) //nolint:gosec
}

// assembleOpenAPILibrary synthesizes a basev0.Endpoint carrying the raw
// OpenAPI document (the only thing proto.GenerateOpenAPI reads) and runs it.
// OpenAPI clients have no facade: proto.GenerateOpenAPI's contract-derived
// codegen is a plain typed HTTP client.
func assembleOpenAPILibrary(ctx context.Context, lang languages.Language, langDir string, entry *ContractEntry, goModule, npmPackage string) (generatedLanguageExport, error) {
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
		Name:    entry.Endpoint.Endpoint,
		Service: entry.Endpoint.Service,
		Module:  defaultFacadeModule,
		Api:     standards.REST,
		ApiDetails: &basev0.API{
			Value: &basev0.API_Rest{Rest: &basev0.RestAPI{Openapi: entry.ContractBytes}},
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
	if err := writeTypeScriptScaffold(langDir, npmPackage, false); err != nil {
		return generatedLanguageExport{}, err
	}
	if err := writeTypeScriptIndex(langDir, false); err != nil {
		return generatedLanguageExport{}, err
	}
	return generatedLanguageExport{Name: "typescript", Path: "typescript/", Exports: []string{npmPackage}}, nil
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
// living under pyPackage._gen. Idempotent: a line already rewritten to
// newPrefix.<pkg>... no longer starts with a bare pkg, so a second pass over
// the same file (each entry's merge rewrites the whole package directory) is
// a no-op for lines an earlier entry already touched.
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

// resolveAndVerifyGoLibrary resolves the generated library's dependencies and
// compiles it. Both steps are hard failures, because neither the module set
// nor the import paths of the generated bindings are knowable from writeGoModule
// alone.
//
// The bindings import whatever the contract's shared proto modules resolve to
// upstream (buf.build/gen/go/..., google.golang.org/genproto/...), which reach
// go.mod only through `go mod tidy`; and they only resolve at all while buf's
// managed mode keeps those modules' go_package pointing upstream, which this
// command asks for by stamping module identities onto the image but cannot
// itself enforce — the except list lives in the companion's buf.gen.yaml. If
// either half stops holding, the output is a library that does not compile,
// and a published library version is immutable. Compiling it here is what
// turns that into a failed command instead of a broken release.
func resolveAndVerifyGoLibrary(dir string) error {
	binary, err := exec.LookPath("go")
	if err != nil {
		return fmt.Errorf("go must be on PATH to resolve and verify the generated Go library at %s: %w", dir, err)
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"build", "./..."}} {
		cmd := exec.Command(binary, args...)
		cmd.Dir = dir
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if runErr := cmd.Run(); runErr != nil {
			return fmt.Errorf("generated Go library at %s does not build (`go %s`): %w\n%s",
				dir, strings.Join(args, " "), runErr, out.String())
		}
	}
	return nil
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

// writeTypeScriptScaffold writes package.json and tsconfig.json once. With
// facadePackages, package.json also exports each facade package by its
// directory ("<package>/<module>"), the import a Go consumer writes as
// "<module path>/<module>".
func writeTypeScriptScaffold(langDir, npmPackage string, facadePackages bool) error {
	subpaths := ""
	if facadePackages {
		subpaths = `,
    "./*": {
      "types": "./dist/*/index.d.ts",
      "import": "./dist/*/index.js"
    }`
	}
	packageJSON := fmt.Sprintf(`{
  "name": %q,
  "version": "0.0.1",
  "type": "module",
  "exports": {
    ".": {
      "types": "./dist/index.d.ts",
      "import": "./dist/index.js"
    }%s
  },
  "files": [
    "dist",
    "README.md"
  ],
  "scripts": {
    "build": "tsc -p tsconfig.json"
  },
  "dependencies": {
    "@bufbuild/protobuf": "^2.0.0",
    "@connectrpc/connect": "^2.0.0"
  },
  "devDependencies": {
    "typescript": %q
  },
  "publishConfig": {
    "registry": "https://npm.pkg.github.com"
  }
}
`, npmPackage, subpaths, pinnedTypeScriptVersion)
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
