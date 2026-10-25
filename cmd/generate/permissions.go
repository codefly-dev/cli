package generate

import (
	"bytes"
	"context"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/golang"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

var permissionsOutput string
var permissionsCheck bool

var PermissionsCmd = &cobra.Command{
	Use:   "permissions [module]",
	Short: "Generate typed Go and TypeScript permissions from published contracts",
	Long: `Read the same digest-checked published method policies used by authority delivery.
Generate typed permission constants and a method requirement table in Go and
TypeScript. Run codefly generate contracts first after editing an API.

By default each Go service gets its own table in its own Go module:
<service>/<source-dir>/pkg/gen/permissions, the tree the go-grpc layout
generates its bindings into (source-dir is the Go agent's setting, "code"
unless the service declares another). A service's table holds the methods its
published contracts serve plus the module contract's foreign methods, which
name no service. The TypeScript file sits beside the Go file. A Python service
gets no table: nothing in it reads one, since the authority's hop in front of it
decides every call, so it is skipped with a note. Any other service that
publishes a contract but has no go.mod at its Go source root fails by name.
--output writes the whole module's table to one directory instead.
--check compares every generated file without writing. Missing or contradictory
policies and colliding identifiers fail by name. This validates vocabulary,
not a person's resource access, installation or consent.`,
	Example: "codefly generate permissions\ncodefly generate permissions --check\ncodefly generate permissions --output services/api/code/pkg/gen/permissions",
	Args:    cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		var module *resources.Module
		var err error
		if len(args) == 0 {
			module, err = common.LoadModule(ctx)
		} else {
			workspace, loadErr := common.LoadWorkspace(ctx)
			if loadErr != nil {
				return loadErr
			}
			module, err = workspace.LoadModuleFromName(ctx, args[0])
		}
		if err != nil {
			return err
		}
		return generatePermissions(ctx, module, permissionsOutput, permissionsCheck)
	},
}

func init() {
	PermissionsCmd.Flags().StringVar(&permissionsOutput, "output", "", "write the whole module's table to this one directory (default: each Go service's <source-dir>/pkg/gen/permissions)")
	PermissionsCmd.Flags().BoolVar(&permissionsCheck, "check", false, "fail on missing or stale output without writing")
}

func generatePermissions(ctx context.Context, module *resources.Module, output string, check bool) error {
	destinations, skipped, err := permissionDestinations(ctx, module, output)
	if err != nil {
		return err
	}
	for _, service := range skipped {
		wool.Get(ctx).In("generatePermissions").Info("no permissions table for a Python service: nothing in it reads one",
			wool.Field("service", service))
	}
	for _, destination := range destinations {
		files, err := permissionFiles(destination.methods)
		if err != nil {
			return err
		}
		for _, name := range []string{"permissions_gen.go", "permissions_gen.ts"} {
			path := filepath.Join(destination.dir, name)
			if check {
				actual, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(actual, files[name]) {
					return fmt.Errorf("%s is missing or stale; run codefly generate permissions", path)
				}
			} else if err := shared.WriteFileAtomic(ctx, path, files[name], 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// permissionDestination is one directory and the method table it holds.
type permissionDestination struct {
	dir     string
	methods []*compositionv1.MethodPolicyV1
}

// permissionDestinations places the module's method policies. An explicit
// output holds the whole module's table. Otherwise each Go service gets the
// methods its own published contracts serve, plus the foreign methods the
// module contract declares without naming a service, in the generated tree
// of its own Go module, so the service imports them with no flag. A Python
// service that publishes policies is skipped and named in skipped: nothing in
// it reads a table, because the hop in front of it decides every call.
func permissionDestinations(ctx context.Context, module *resources.Module, output string) (destinations []permissionDestination, skipped []string, err error) {
	if output != "" {
		methods, declared, loadErr := gitops.MethodPolicies(module.Dir())
		if loadErr != nil {
			return nil, nil, loadErr
		}
		if !declared {
			return nil, nil, fmt.Errorf("%s publishes no policies; run codefly generate contracts", module.Dir())
		}
		return []permissionDestination{{dir: output, methods: methods}}, nil, nil
	}
	served, declared, err := gitops.ServiceMethodPolicies(module.Dir())
	if err != nil {
		return nil, nil, err
	}
	if !declared {
		return nil, nil, fmt.Errorf("%s publishes no policies; run codefly generate contracts", module.Dir())
	}
	services, err := module.LoadServices(ctx)
	if err != nil {
		return nil, nil, err
	}
	placed := map[string]bool{}
	for _, service := range services {
		own := served.ByService[service.Name]
		if isPythonService(service) {
			if len(own) > 0 {
				placed[service.Name] = true
				skipped = append(skipped, service.Name)
			}
			continue
		}
		root, isGo, err := goModuleRoot(service)
		if err != nil {
			return nil, nil, err
		}
		if !isGo {
			if len(own) > 0 {
				return nil, nil, fmt.Errorf("service %s publishes method policies but has no go.mod at %s; pass --output", service.Name, root)
			}
			continue
		}
		methods := append(append([]*compositionv1.MethodPolicyV1(nil), own...), served.Foreign...)
		if len(methods) == 0 {
			continue
		}
		placed[service.Name] = true
		destinations = append(destinations, permissionDestination{dir: filepath.Join(root, "pkg", "gen", "permissions"), methods: methods})
	}
	for _, name := range served.Services {
		if !placed[name] {
			return nil, nil, fmt.Errorf("service %s publishes method policies but is not a service of module %s; pass --output", name, module.Name)
		}
	}
	if len(destinations) == 0 && len(skipped) == 0 {
		return nil, nil, fmt.Errorf("module %s declares method policies but has no Go service to generate them into; pass --output", module.Name)
	}
	return destinations, skipped, nil
}

// isPythonService reports whether a service's agent builds Python
// (python-fastapi, python-grpc, ...).
func isPythonService(service *resources.Service) bool {
	return service.Agent != nil && strings.HasPrefix(service.Agent.Name, "python")
}

// goModuleRoot is where a service's go.mod lives: the module root of the Go
// agent's source-dir (core's golang settings, "code" unless the service
// declares another), below the service directory. isGo reports whether a
// go.mod is there.
func goModuleRoot(service *resources.Service) (root string, isGo bool, err error) {
	var settings golang.GoAgentSettings
	if err := service.LoadSettingsFromSpec(&settings); err != nil {
		return "", false, fmt.Errorf("service %s: %w", service.Name, err)
	}
	if err := settings.Validate(); err != nil {
		return "", false, fmt.Errorf("service %s: %w", service.Name, err)
	}
	moduleRoot, _ := golang.SplitSourceDir(filepath.ToSlash(settings.GoSourceDir()))
	root = filepath.Join(service.Dir(), filepath.FromSlash(moduleRoot))
	info, statErr := os.Stat(filepath.Join(root, "go.mod"))
	return root, statErr == nil && !info.IsDir(), nil
}

func permissionIdentifier(raw string) string {
	var b strings.Builder
	upper := true
	for _, r := range raw {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			upper = true
			continue
		}
		if upper {
			r = unicode.ToUpper(r)
			upper = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

func permissionFiles(methods []*compositionv1.MethodPolicyV1) (map[string][]byte, error) {
	if err := gitops.ValidateMethodPolicies(methods); err != nil {
		return nil, err
	}
	names := map[string]string{}
	values := map[string]string{}
	for _, m := range methods {
		for _, sc := range m.GetRequires() {
			for _, action := range sc.GetActions() {
				value := sc.GetResourceKind() + ":" + action
				name := permissionIdentifier(value)
				switch name {
				case "Permission", "MethodPolicy", "Methods":
					return nil, fmt.Errorf("permission %q collides with generated identifier %s", value, name)
				}
				if previous, ok := names[name]; ok && previous != value {
					return nil, fmt.Errorf("permissions %q and %q collide as %s", previous, value, name)
				}
				names[name] = value
				values[value] = name
			}
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	var goCode, ts strings.Builder
	goCode.WriteString("// Code generated by codefly generate permissions. DO NOT EDIT.\npackage permissions\n\nimport \"strings\"\n\ntype Permission string\nfunc (p Permission) Kind() string { k, _, _ := strings.Cut(string(p),\":\"); return k }\nfunc (p Permission) Action() string { _, a, _ := strings.Cut(string(p),\":\"); return a }\nconst (\n")
	ts.WriteString("// Code generated by codefly generate permissions. DO NOT EDIT.\nexport const Permission = {\n")
	for _, name := range ordered {
		fmt.Fprintf(&goCode, "%s Permission = %q\n", name, names[name])
		fmt.Fprintf(&ts, "  %s: %q,\n", name, names[name])
	}
	goCode.WriteString(")\ntype MethodPolicy struct { AnyCheckedCaller bool; Requires []Permission }\nfunc Methods() map[string]MethodPolicy { return map[string]MethodPolicy{\n")
	ts.WriteString("} as const;\nexport type Permission = typeof Permission[keyof typeof Permission];\nexport const Methods = {\n")
	sorted := append([]*compositionv1.MethodPolicyV1(nil), methods...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].GetMethod() < sorted[j].GetMethod() })
	for _, m := range sorted {
		required := map[string]bool{}
		for _, sc := range m.GetRequires() {
			for _, action := range sc.GetActions() {
				required[values[sc.GetResourceKind()+":"+action]] = true
			}
		}
		refs := make([]string, 0, len(required))
		for name := range required {
			refs = append(refs, name)
		}
		sort.Strings(refs)
		fmt.Fprintf(&goCode, "%s: {AnyCheckedCaller:%t, Requires: []Permission{%s}},\n", strconv.Quote(m.GetMethod()), m.GetAnyCheckedCaller(), strings.Join(refs, ","))
		tsRefs := make([]string, 0, len(refs))
		for _, name := range refs {
			tsRefs = append(tsRefs, "Permission."+name)
		}
		fmt.Fprintf(&ts, "%s: { anyCheckedCaller: %t, requires: [%s] },\n", strconv.Quote(m.GetMethod()), m.GetAnyCheckedCaller(), strings.Join(tsRefs, ","))
	}
	goCode.WriteString("} }\n")
	ts.WriteString("} as const;\n")
	formatted, err := format.Source([]byte(goCode.String()))
	if err != nil {
		return nil, err
	}
	return map[string][]byte{"permissions_gen.go": formatted, "permissions_gen.ts": []byte(ts.String())}, nil
}
