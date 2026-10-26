package generators

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// typeScriptPackagesDir holds one generated barrel per proto package, under
// src/ (which this command owns whole), beside gen/ (which the plugins own).
const typeScriptPackagesDir = "packages"

var (
	// The header protoc-gen-es writes on every file. A file with no package
	// clause (or none of this header) declares into protobuf's root scope.
	typeScriptProtoPackageRe = regexp.MustCompile(`^// @generated from file .* \(package ([A-Za-z0-9_.]+),`)
	// One top-level declaration export: the only export form the protobuf,
	// facade and OpenAPI plugins emit. Anything else is refused rather than
	// guessed at, since a misread name is an ambiguous or missing export.
	typeScriptExportRe = regexp.MustCompile(`^export\s+(?:declare\s+)?(const\s+enum|enum|const|let|var|function\*?|async\s+function|abstract\s+class|class|type|interface|namespace)\s+([A-Za-z_$][\w$]*)`)
)

// typeScriptModule is one generated file the entry point reaches.
type typeScriptModule struct {
	path    string          // relative to src/, slash-separated, with .ts
	facade  bool            // outside gen/: a hoisted facade
	pkg     string          // proto package; "" for facades and package-less files
	exports map[string]bool // exported name -> type-only
}

// typeScriptOwner is one claimant of a top-level name in the entry point:
// a package's declaration (pkg set), which can yield the flat name to its
// namespace, or anything else (a facade, a namespace, a package-less file),
// which cannot.
type typeScriptOwner struct {
	pkg   string
	label string
}

func (m typeScriptModule) specifier(fromDir string) string {
	rel := strings.TrimSuffix(m.path, ".ts") + ".js"
	if fromDir == "" {
		return "./" + rel
	}
	return "../" + rel
}

// writeTypeScriptIndex writes src/index.ts: the barrel package.json's
// "exports" names. It is generated output, not a scaffold — src/ is wholly
// owned by this command (wipeTypeScriptLibrary removes it) and which files
// exist depends on the contracts generated — so it is rewritten on every run.
//
// Protobuf keeps names unique within a package, not across the dependency
// graph (codefly.base.v0 and saas.accounts.v1 may both declare Settings), so a
// barrel of plain `export *` lines is ambiguous (TS2308). The shape:
//   - every facade is re-exported flat, under the name its plugin gave it;
//   - every proto package is one namespace named from the package
//     (saas.accounts.v1 -> saas_accounts_v1_pb), through a barrel in
//     src/packages/, so moving a message between files of a package renames
//     nothing;
//   - every binding name nothing else claims is also re-exported flat. A
//     package's declaration of a name another package, a facade or a
//     namespace also claims is left out of the flat set and listed in a
//     comment at the top of the barrel; its package namespace reaches it.
//     Two facades or package-less files on one name are refused, since
//     nothing else would reach either.
func writeTypeScriptIndex(langDir string) error {
	srcDir := filepath.Join(langDir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return err
	}
	modules, err := readTypeScriptModules(srcDir)
	if err != nil {
		return err
	}
	index, err := newTypeScriptIndex(modules)
	if err != nil {
		return err
	}
	if err := index.writePackageBarrels(srcDir); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(srcDir, "index.ts"), []byte(index.render()), 0o644) //nolint:gosec
}

// typeScriptIndex is the entry point's plan: which package each binding
// belongs to, each package's namespace, and who claims every top-level name.
type typeScriptIndex struct {
	modules    []typeScriptModule
	packages   map[string][]typeScriptModule // proto package -> its files
	pkgNames   []string                      // sorted keys of packages
	namespaces map[string]string             // proto package -> namespace identifier
	owners     map[string][]typeScriptOwner  // top-level name -> its claimants
}

func newTypeScriptIndex(modules []typeScriptModule) (*typeScriptIndex, error) {
	index := &typeScriptIndex{
		modules:    modules,
		packages:   map[string][]typeScriptModule{},
		namespaces: map[string]string{},
		owners:     map[string][]typeScriptOwner{},
	}
	for _, module := range modules {
		if module.pkg != "" {
			index.packages[module.pkg] = append(index.packages[module.pkg], module)
		} else {
			// A facade or a package-less file: its names have no other way in.
			for name := range module.exports {
				index.claim(name, typeScriptOwner{label: module.path})
			}
		}
	}
	for pkg := range index.packages {
		index.pkgNames = append(index.pkgNames, pkg)
	}
	sort.Strings(index.pkgNames)
	for _, pkg := range index.pkgNames {
		namespace := typeScriptPackageNamespace(pkg)
		index.namespaces[pkg] = namespace
		index.claim(namespace, typeScriptOwner{label: "the " + pkg + " namespace"})
		declaredIn := map[string]string{}
		for _, module := range index.packages[pkg] {
			for name := range module.exports {
				if other, dup := declaredIn[name]; dup {
					return nil, fmt.Errorf("package %s exports %s from both %s and %s", pkg, name, other, module.path)
				}
				declaredIn[name] = module.path
				index.claim(name, typeScriptOwner{pkg: pkg, label: pkg})
			}
		}
	}

	// Only a package's declaration can step out of the flat set, because its
	// namespace still reaches it. A facade, a namespace and a package-less
	// file have no other way in, so two of them on one name is refused.
	var refusals []string
	for name, claimants := range index.owners {
		var fixed []string
		for _, owner := range claimants {
			if owner.pkg == "" {
				fixed = append(fixed, owner.label)
			}
		}
		if len(fixed) > 1 {
			sort.Strings(fixed)
			refusals = append(refusals, fmt.Sprintf("%s (%s)", name, strings.Join(fixed, ", ")))
		}
	}
	if len(refusals) > 0 {
		sort.Strings(refusals)
		return nil, fmt.Errorf("the TypeScript entry point cannot export these names unambiguously: %s", strings.Join(refusals, "; "))
	}
	return index, nil
}

func (x *typeScriptIndex) claim(name string, owner typeScriptOwner) {
	x.owners[name] = append(x.owners[name], owner)
}

func (x *typeScriptIndex) collides(name string) bool { return len(x.owners[name]) > 1 }

func (x *typeScriptIndex) render() string {
	var content strings.Builder
	content.WriteString("// generated by codefly generate client; do not edit\n")
	content.WriteString(x.collisionComment())
	for _, module := range x.modules {
		if module.facade {
			fmt.Fprintf(&content, "export * from %q;\n", module.specifier(""))
		}
	}
	for _, pkg := range x.pkgNames {
		fmt.Fprintf(&content, "export * as %s from %q;\n", x.namespaces[pkg], "./"+typeScriptPackagesDir+"/"+pkg+".js")
	}
	for _, module := range x.modules {
		if !module.facade {
			content.WriteString(x.flatExport(module))
		}
	}
	return content.String()
}

// collisionComment names every name more than one owner claims, with each
// package namespace that reaches it.
func (x *typeScriptIndex) collisionComment() string {
	var collided []string
	for name := range x.owners {
		if x.collides(name) {
			collided = append(collided, name)
		}
	}
	if len(collided) == 0 {
		return ""
	}
	sort.Strings(collided)
	var content strings.Builder
	content.WriteString("//\n// Names more than one package (or a facade) declares. No package's declaration\n// is re-exported flat under them; reach each through its package namespace:\n")
	for _, name := range collided {
		var spaces []string
		for _, owner := range x.owners[name] {
			if owner.pkg != "" {
				spaces = append(spaces, x.namespaces[owner.pkg]+"."+name)
			}
		}
		sort.Strings(spaces)
		fmt.Fprintf(&content, "//   %s: %s\n", name, strings.Join(spaces, ", "))
	}
	return content.String()
}

// flatExport re-exports a binding's names flat: the whole file when none of
// them collide, otherwise each name but the colliding ones, a type-only name
// marked so isolatedModules emits no value import for it.
func (x *typeScriptIndex) flatExport(module typeScriptModule) string {
	var flat []string
	whole := true
	for name, typeOnly := range module.exports {
		switch {
		case module.pkg != "" && x.collides(name):
			whole = false
		case typeOnly:
			flat = append(flat, "type "+name)
		default:
			flat = append(flat, name)
		}
	}
	if whole {
		return fmt.Sprintf("export * from %q;\n", module.specifier(""))
	}
	if len(flat) == 0 {
		return ""
	}
	sort.Slice(flat, func(i, j int) bool {
		return strings.TrimPrefix(flat[i], "type ") < strings.TrimPrefix(flat[j], "type ")
	})
	return fmt.Sprintf("export {\n  %s,\n} from %q;\n", strings.Join(flat, ",\n  "), module.specifier(""))
}

// typeScriptPackageNamespace names a proto package's namespace: its
// components joined by "_" with each "_" inside a component doubled, so
// a_b.v1 (a__b_v1_pb) and a.b_v1 (a_b__v1_pb) stay distinct, and the "_pb"
// suffix of protoc-gen-es's own modules, which keeps it clear of the
// facade a single-component package is usually named after (package api,
// facade api). A residual clash is caught as a collision, never emitted.
func typeScriptPackageNamespace(pkg string) string {
	components := strings.Split(pkg, ".")
	for i, component := range components {
		components[i] = strings.ReplaceAll(component, "_", "__")
	}
	return strings.Join(components, "_") + "_pb"
}

// writePackageBarrels writes src/packages/<proto package>.ts per package:
// every file of the package, re-exported whole, which protobuf's uniqueness
// within a package makes unambiguous. The file is named for the package, not
// the namespace, and never ends in _pb.ts: that suffix marks plugin output,
// which consumers hold to the plugin's own stamp.
func (x *typeScriptIndex) writePackageBarrels(srcDir string) error {
	dir := filepath.Join(srcDir, typeScriptPackagesDir)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if len(x.pkgNames) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, pkg := range x.pkgNames {
		var content strings.Builder
		fmt.Fprintf(&content, "// generated by codefly generate client from proto package %s; do not edit\n", pkg)
		for _, module := range x.packages[pkg] {
			fmt.Fprintf(&content, "export * from %q;\n", module.specifier(typeScriptPackagesDir))
		}
		if err := os.WriteFile(filepath.Join(dir, pkg+".ts"), []byte(content.String()), 0o644); err != nil { //nolint:gosec
			return err
		}
	}
	return nil
}

// readTypeScriptModules reads every generated file under srcDir except the
// entry point and the package barrels this command writes, sorted by path.
func readTypeScriptModules(srcDir string) ([]typeScriptModule, error) {
	var modules []typeScriptModule
	err := filepath.WalkDir(srcDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, relErr := filepath.Rel(srcDir, path)
		if relErr != nil {
			return relErr
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if relative == typeScriptPackagesDir {
				return filepath.SkipDir
			}
			return nil
		}
		if relative == "index.ts" || !strings.HasSuffix(relative, ".ts") {
			return nil
		}
		module, readErr := readTypeScriptModule(path, relative)
		if readErr != nil {
			return readErr
		}
		modules = append(modules, module)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].path < modules[j].path })
	return modules, nil
}

func readTypeScriptModule(path, relative string) (typeScriptModule, error) {
	data, err := os.ReadFile(path) //nolint:gosec // a file this command just generated
	if err != nil {
		return typeScriptModule{}, err
	}
	module := typeScriptModule{
		path:    relative,
		facade:  !strings.HasPrefix(relative, "gen/"),
		exports: map[string]bool{},
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), len(data)+1)
	for scanner.Scan() {
		line := scanner.Text()
		if match := typeScriptProtoPackageRe.FindStringSubmatch(line); match != nil && !module.facade && module.pkg == "" {
			module.pkg = match[1]
		}
		if !strings.HasPrefix(line, "export") {
			continue
		}
		match := typeScriptExportRe.FindStringSubmatch(line)
		if match == nil {
			return typeScriptModule{}, fmt.Errorf("cannot read the exports of generated %s: %q is not a declaration export", relative, line)
		}
		typeOnly := match[1] == "type" || match[1] == "interface"
		// A name declared as both (an interface merged with a class) is a value.
		if previous, seen := module.exports[match[2]]; seen {
			typeOnly = typeOnly && previous
		}
		module.exports[match[2]] = typeOnly
	}
	return module, scanner.Err()
}
