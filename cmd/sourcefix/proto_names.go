package sourcefix

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

var protoNamesDryRun bool

// ProtoNamesCmd moves a service's contract out of the scaffold's shared
// names.
var ProtoNamesCmd = &cobra.Command{
	Use:   "proto-names [service]",
	Short: "Give a service's protobuf contract its own package and path",
	Long: `Give a service's protobuf contract its own package and path.

A scaffolded service keeps its contract in proto/api.proto, package api. Two
such contracts cannot be loaded in one process: a service that depends on
another's generated client registers both, and the protobuf runtime refuses the
second (in Go, a panic at start: proto: file "api.proto" is already registered).
` + "`codefly run`, `lint`, `test` and `doctor workspace`" + ` report it before that.

This command moves the contract to the versioned layout:

  proto/api.proto, package api
    -> proto/<module>/<service>/v1/api.proto, package <module>.<service>.v1
       (proto/<service>/v1, package <service>.v1 when module and service share a name)

and, for a Go service, points go_package and the service's own imports at the
bindings' new directory. The procedure paths change with the package
(/api.Service/Method becomes /<package>.Service/Method); REST routes do not.

Then regenerate what derives from the contract:

  codefly sync service <service>
  codefly generate contracts <module>
  codefly generate client --from <module>/<service>/<endpoint>   # each client of it
`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		_, module, service, err := common.LoadRequiredE(ctx, args)
		if err != nil {
			return err
		}
		plan, err := PlanProtoNames(service.Dir(), module.Name, service.Name)
		if err != nil {
			return fmt.Errorf("cannot move the contract of %s/%s: %w", module.Name, service.Name, err)
		}
		for _, line := range plan.Describe() {
			cli.Header(2, "%s", line)
		}
		if protoNamesDryRun {
			return nil
		}
		if err := plan.Apply(); err != nil {
			return err
		}
		cli.Header(2, "Next: codefly sync service %s, then codefly generate contracts %s and regenerate each client of it", service.Name, module.Name)
		return ctx.Err()
	},
}

func init() {
	ProtoNamesCmd.Flags().BoolVar(&protoNamesDryRun, "dry-run", false, "Show what would change without changing it")
}

// ProtoNamesPlan is the move of one service's contract to its own names.
type ProtoNamesPlan struct {
	dir string
	// From and To are the proto file, relative to the service.
	From, To string
	// OldPackage and Package are the protobuf packages.
	OldPackage, Package string
	// OldGoImport and GoImport are the bindings' Go import paths, and
	// GoName their package name; empty when the contract declares no
	// go_package.
	OldGoImport, GoImport, GoName string
	// GoFiles are the service's own Go files that import the bindings.
	GoFiles []string

	proto string
}

var (
	protoPackageDecl   = regexp.MustCompile(`(?m)^(\s*package\s+)([A-Za-z_][\w.]*)(\s*;)`)
	protoGoPackageDecl = regexp.MustCompile(`(?m)^(\s*option\s+go_package\s*=\s*")([^";]+)(;([A-Za-z_]\w*))?("\s*;)`)
	notProtoIdent      = regexp.MustCompile(`[^a-z0-9_]+`)
)

// protoIdent spells a module or service name as a protobuf package element.
func protoIdent(name string) string {
	ident := strings.Trim(notProtoIdent.ReplaceAllString(strings.ToLower(name), "_"), "_")
	if ident == "" || (ident[0] >= '0' && ident[0] <= '9') {
		ident = "s" + ident
	}
	return ident
}

// PlanProtoNames reads a service laid out as the scaffold leaves it and says
// what moving its contract changes. It refuses a service that has no
// proto/api.proto: one already in a versioned layout, or one with no contract.
func PlanProtoNames(serviceDir, module, service string) (*ProtoNamesPlan, error) {
	from := filepath.Join("proto", "api.proto")
	content, err := os.ReadFile(filepath.Join(serviceDir, from))
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("it has no proto/api.proto: its contract is not in the scaffold's layout (already moved, or none)")
	}
	if err != nil {
		return nil, err
	}
	pkg := protoPackageDecl.FindSubmatch(content)
	if pkg == nil {
		return nil, fmt.Errorf("proto/api.proto declares no package")
	}
	elements := []string{protoIdent(module), protoIdent(service), "v1"}
	if elements[0] == elements[1] {
		elements = elements[1:]
	}
	plan := &ProtoNamesPlan{dir: serviceDir, From: from, OldPackage: string(pkg[2]), Package: strings.Join(elements, ".")}
	plan.To = filepath.Join(append(append([]string{"proto"}, elements...), "api.proto")...)
	if _, err := os.Stat(filepath.Join(serviceDir, plan.To)); err == nil {
		return nil, fmt.Errorf("%s already exists", plan.To)
	}
	updated := protoPackageDecl.ReplaceAll(content, []byte("${1}"+plan.Package+"${3}"))
	if goPackage := protoGoPackageDecl.FindSubmatch(content); goPackage != nil {
		plan.OldGoImport = string(goPackage[2])
		plan.GoImport = plan.OldGoImport + "/" + strings.Join(elements, "/")
		plan.GoName = string(goPackage[4])
		if plan.GoName == "" {
			plan.GoName = filepath.Base(plan.OldGoImport)
		}
		updated = protoGoPackageDecl.ReplaceAll(updated, []byte("${1}"+plan.GoImport+";"+plan.GoName+"${5}"))
		if plan.GoFiles, err = goFilesImporting(serviceDir, plan.OldGoImport); err != nil {
			return nil, err
		}
	}
	plan.proto = string(updated)
	return plan, nil
}

// Describe says what the plan changes, one line each.
func (p *ProtoNamesPlan) Describe() []string {
	lines := []string{
		fmt.Sprintf("%s -> %s", p.From, p.To),
		fmt.Sprintf("package %s -> %s", p.OldPackage, p.Package),
	}
	if p.GoImport != "" {
		lines = append(lines, fmt.Sprintf("go_package %s -> %s", p.OldGoImport, p.GoImport))
		for _, file := range p.GoFiles {
			lines = append(lines, "imports in "+file)
		}
	}
	return lines
}

// Apply writes the contract at its new path, removes the old one, and
// points the service's own Go imports at the bindings' new directory. The
// bindings themselves are the agent's: `codefly sync service` regenerates
// them and its adapters from the moved contract.
func (p *ProtoNamesPlan) Apply() error {
	to := filepath.Join(p.dir, p.To)
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(to, []byte(p.proto), 0o644); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(p.dir, p.From)); err != nil {
		return err
	}
	for _, file := range p.GoFiles {
		path := filepath.Join(p.dir, file)
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(rewriteGoImports(string(content), p.OldGoImport, p.GoImport, p.GoName)), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// rewriteGoImports points imports of the bindings (and of packages under
// them, such as the Connect handlers) at their new directory. An import of
// the bindings with no name gets one: the directory now ends in the version,
// the package keeps its name.
func rewriteGoImports(source, oldImport, newImport, name string) string {
	quoted := regexp.QuoteMeta(oldImport)
	sub := regexp.MustCompile(`"` + quoted + `/`)
	source = sub.ReplaceAllString(source, `"`+newImport+`/`)
	exact := regexp.MustCompile(`(?m)^(\s*)(?:import\s+)?(([A-Za-z_]\w*|\.)\s+)?"` + quoted + `"`)
	return exact.ReplaceAllStringFunc(source, func(line string) string {
		m := exact.FindStringSubmatch(line)
		keyword := ""
		if strings.Contains(line[:strings.Index(line, `"`)], "import") {
			keyword = "import "
		}
		alias := m[2]
		if alias == "" {
			alias = name + " "
		}
		return m[1] + keyword + alias + `"` + newImport + `"`
	})
}

// goFilesImporting lists the service's Go files that import the bindings or
// a package under them, leaving out the bindings themselves and files a
// generator owns (they are regenerated).
func goFilesImporting(serviceDir, goImport string) ([]string, error) {
	needle := regexp.MustCompile(`"` + regexp.QuoteMeta(goImport) + `[/"]`)
	var out []string
	err := filepath.WalkDir(serviceDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if name := entry.Name(); name == "gen" || name == "node_modules" || (strings.HasPrefix(name, ".") && path != serviceDir) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_gen.go") || strings.HasSuffix(path, ".pb.go") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if needle.Match(content) {
			rel, err := filepath.Rel(serviceDir, path)
			if err != nil {
				return err
			}
			out = append(out, rel)
		}
		return nil
	})
	return out, err
}
