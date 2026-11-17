package generate

import (
	"bytes"
	"context"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/golang"
	"github.com/codefly-dev/core/runners/python"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

var permissionsOutput string
var permissionsCheck bool

var PermissionsCmd = &cobra.Command{
	Use:   "permissions [module]",
	Short: "Generate typed Go, TypeScript and Python permissions from published contracts",
	Long: `Read the same digest-checked published method policies used by authority delivery.
Generate typed permission constants and a method requirement table in Go and
TypeScript, and in Python for a Python service. A permission a call may carry
beyond what it requires is a constant too, and its method's row names it apart
from what the call requires. Run codefly generate contracts first after editing
an API.

By default each Go service gets its own table in its own Go module:
<service>/<source-dir>/pkg/gen/permissions, the tree the go-grpc layout
generates its bindings into (source-dir is the Go agent's setting, "code"
unless the service declares another). A service's table holds the methods its
published contracts serve plus foreign methods selected for that service.
Unselected foreign methods appear in every Go service's table for compatibility.
What an endpoint serves is what the contract catalog records
for it (the declared surface, or every service of its package); a method a
published descriptor declares that no endpoint serves, such as a protocol other
modules serve, is left out with a note and needs no policy. The TypeScript file
sits beside the Go file. A Python service (a python-* agent) gets its own
table as one Python file in its own source tree,
<service>/<source-dir>/src/gen/permissions_gen.py (source-dir is "code" unless
the service declares another, and is where pyproject.toml lives): the same
constants and rows, for the verifier it hands its declarations to. A Go
service that publishes a contract but has no go.mod at its Go source root, and
a Python one with no pyproject.toml at its source root, fail by name.
--output writes the whole module's table, in Go and TypeScript, to one
directory instead.
--check compares every generated file without writing. A served method with no
policy, contradictory policies and colliding identifiers fail by name. This validates vocabulary,
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
	PermissionsCmd.Flags().StringVar(&permissionsOutput, "output", "", "write the whole module's table to this one directory (default: each Go service's <source-dir>/pkg/gen/permissions and each Python service's <source-dir>/src/gen)")
	PermissionsCmd.Flags().BoolVar(&permissionsCheck, "check", false, "fail on missing or stale output without writing")
}

func generatePermissions(ctx context.Context, module *resources.Module, output string, check bool) error {
	destinations, unserved, err := permissionDestinations(ctx, module, output)
	if err != nil {
		return err
	}
	if len(unserved) > 0 {
		wool.Get(ctx).In("generatePermissions").Info("not in any permissions table: a published descriptor declares these methods but no endpoint of the module serves them",
			wool.Field("methods", strings.Join(unserved, ", ")))
	}
	for _, destination := range destinations {
		files, err := permissionFiles(destination.methods)
		if err != nil {
			return err
		}
		for _, name := range destination.files {
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
	dir string
	// files are the generated files the directory holds, by name.
	files   []string
	methods []*compositionv1.MethodPolicyV1
}

// goPermissionFiles is what a Go service, and an explicit output, holds;
// pythonPermissionFiles what a Python service holds.
var (
	goPermissionFiles     = []string{"permissions_gen.go", "permissions_gen.ts"}
	pythonPermissionFiles = []string{"permissions_gen.py"}
)

// permissionDestinations places the module's method policies, and names the
// published methods no endpoint serves, which no table holds. An explicit
// output holds the whole module's table. Otherwise each Go service gets the
// methods its own published contracts serve and foreign methods selected for it,
// plus legacy foreign methods with no service selector, in the generated tree
// of its own Go module, so the service imports them with no flag. A Python
// service gets the same methods as one Python file in its own source tree.
func permissionDestinations(ctx context.Context, module *resources.Module, output string) (destinations []permissionDestination, unserved []string, err error) {
	served, declared, err := gitops.ServiceMethodPolicies(module.Dir())
	if err != nil {
		return nil, nil, err
	}
	if !declared {
		return nil, nil, fmt.Errorf("%s publishes no policies; run codefly generate contracts", module.Dir())
	}
	if output != "" {
		methods, tableErr := served.All()
		if tableErr != nil {
			return nil, nil, tableErr
		}
		return []permissionDestination{{dir: output, files: goPermissionFiles, methods: methods}}, served.Unserved, nil
	}
	services, err := module.LoadServices(ctx)
	if err != nil {
		return nil, nil, err
	}
	placed := map[string]bool{}
	for _, service := range services {
		own := served.ByService[service.Name]
		if isPythonService(service) {
			if len(own) == 0 {
				continue
			}
			root, isProject, err := pythonProjectRoot(service)
			if err != nil {
				return nil, nil, err
			}
			if !isProject {
				return nil, nil, fmt.Errorf("service %s publishes method policies but has no pyproject.toml at %s; pass --output", service.Name, root)
			}
			placed[service.Name] = true
			destinations = append(destinations, permissionDestination{
				dir:     filepath.Join(root, "src", "gen"),
				files:   pythonPermissionFiles,
				methods: append(append([]*compositionv1.MethodPolicyV1(nil), own...), served.Foreign...),
			})
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
		destinations = append(destinations, permissionDestination{dir: filepath.Join(root, "pkg", "gen", "permissions"), files: goPermissionFiles, methods: methods})
	}
	for _, name := range served.Services {
		if !placed[name] {
			return nil, nil, fmt.Errorf("service %s publishes method policies but is not a service of module %s; pass --output", name, module.Name)
		}
	}
	if len(destinations) == 0 {
		return nil, nil, fmt.Errorf("module %s declares method policies but has no Go or Python service to generate them into; pass --output", module.Name)
	}
	return destinations, served.Unserved, nil
}

// isPythonService reports whether a service's agent builds Python
// (python-fastapi, python-grpc, ...).
func isPythonService(service *resources.Service) bool {
	return service.Agent != nil && strings.HasPrefix(service.Agent.Name, "python")
}

// pythonSourceDir is where the Python agents keep a service's project when
// the service declares no source-dir.
const pythonSourceDir = "code"

// pythonProjectRoot is where a Python service's pyproject.toml lives: its
// source-dir (core's python settings), "code" unless the service declares
// another, below the service directory. isProject reports whether a
// pyproject.toml is there.
func pythonProjectRoot(service *resources.Service) (root string, isProject bool, err error) {
	var settings python.PythonAgentSettings
	if err := service.LoadSettingsFromSpec(&settings); err != nil {
		return "", false, fmt.Errorf("service %s: %w", service.Name, err)
	}
	sourceDir := settings.SourceDir
	if sourceDir == "" {
		sourceDir = pythonSourceDir
	}
	root = filepath.Join(service.Dir(), filepath.FromSlash(sourceDir))
	info, statErr := os.Stat(filepath.Join(root, "pyproject.toml"))
	return root, statErr == nil && !info.IsDir(), nil
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

// pythonPermissionName is a permission's Python constant: its value in upper
// case, every run of anything but letters and digits an underscore.
func pythonPermissionName(raw string) string {
	var b strings.Builder
	separated := false
	for _, r := range raw {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			separated = b.Len() > 0
			continue
		}
		if separated {
			b.WriteRune('_')
			separated = false
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	if name := b.String(); name != "" && unicode.IsDigit(rune(name[0])) {
		return "P_" + name
	}
	return b.String()
}

func permissionFiles(methods []*compositionv1.MethodPolicyV1) (map[string][]byte, error) {
	// The generator is opt-in: a table names every method, so each declares.
	if err := gitops.ValidateMethodPolicies(methods, true); err != nil {
		return nil, err
	}
	names := map[string]string{}
	values := map[string]string{}
	// The Python constants, which fold case where the Go ones do not.
	pythonValues := map[string]string{}
	pythonNames := map[string]string{}
	for _, m := range methods {
		// What a call may carry beyond what it requires is a permission the
		// service performing that effect checks by name, so it is named too.
		for _, sc := range slices.Concat(m.GetRequires(), m.GetCarries()) {
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
				pythonName := pythonPermissionName(value)
				// Kinds and actions are lower case, so two values that are one
				// Python name are one Go name too and were refused above; this
				// holds if that ever stops being true.
				if previous, ok := pythonValues[pythonName]; ok && previous != value {
					return nil, fmt.Errorf("permissions %q and %q collide as %s", previous, value, pythonName)
				}
				pythonValues[pythonName] = value
				pythonNames[value] = pythonName
			}
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	var goCode, ts, py strings.Builder
	py.WriteString(pythonPermissionsHeader)
	pythonOrdered := make([]string, 0, len(pythonValues))
	for name := range pythonValues {
		pythonOrdered = append(pythonOrdered, name)
	}
	sort.Strings(pythonOrdered)
	for _, name := range pythonOrdered {
		kind, action, _ := strings.Cut(pythonValues[name], ":")
		fmt.Fprintf(&py, "%s = Permission(%s, %s)\n", name, strconv.Quote(kind), strconv.Quote(action))
	}
	py.WriteString("\nMETHODS: dict[str, MethodPolicy] = {\n")
	goCode.WriteString("// Code generated by codefly generate permissions. DO NOT EDIT.\npackage permissions\n\nimport \"strings\"\n\ntype Permission string\nfunc (p Permission) Kind() string { k, _, _ := strings.Cut(string(p),\":\"); return k }\nfunc (p Permission) Action() string { _, a, _ := strings.Cut(string(p),\":\"); return a }\nconst (\n")
	ts.WriteString("// Code generated by codefly generate permissions. DO NOT EDIT.\nexport const Permission = {\n")
	for _, name := range ordered {
		fmt.Fprintf(&goCode, "%s Permission = %q\n", name, names[name])
		fmt.Fprintf(&ts, "  %s: %q,\n", name, names[name])
	}
	goCode.WriteString(")\ntype MethodPolicy struct { AnyCheckedCaller bool; Requires []Permission; Carries []Permission }\nfunc Methods() map[string]MethodPolicy { return map[string]MethodPolicy{\n")
	ts.WriteString("} as const;\nexport type Permission = typeof Permission[keyof typeof Permission];\nexport const Methods = {\n")
	sorted := append([]*compositionv1.MethodPolicyV1(nil), methods...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].GetMethod() < sorted[j].GetMethod() })
	// identifiers are the constants naming a list of scopes, each once, sorted.
	identifiers := func(scopes []*authorityv1.WorkScopeV1, values map[string]string) []string {
		var refs []string
		for _, sc := range scopes {
			for _, action := range sc.GetActions() {
				if name := values[sc.GetResourceKind()+":"+action]; !slices.Contains(refs, name) {
					refs = append(refs, name)
				}
			}
		}
		sort.Strings(refs)
		return refs
	}
	tsList := func(refs []string) string {
		qualified := make([]string, 0, len(refs))
		for _, name := range refs {
			qualified = append(qualified, "Permission."+name)
		}
		return strings.Join(qualified, ",")
	}
	for _, m := range sorted {
		requires, carries := identifiers(m.GetRequires(), values), identifiers(m.GetCarries(), values)
		// A Go row names what the call carries only when it carries something,
		// so a table with no such call differs from an older one by its type.
		carried := ""
		if len(carries) > 0 {
			carried = fmt.Sprintf(", Carries: []Permission{%s}", strings.Join(carries, ","))
		}
		fmt.Fprintf(&goCode, "%s: {AnyCheckedCaller:%t, Requires: []Permission{%s}%s},\n", strconv.Quote(m.GetMethod()), m.GetAnyCheckedCaller(), strings.Join(requires, ","), carried)
		fmt.Fprintf(&ts, "%s: { anyCheckedCaller: %t, requires: [%s], carries: [%s] },\n", strconv.Quote(m.GetMethod()), m.GetAnyCheckedCaller(), tsList(requires), tsList(carries))
		anyCheckedCaller := "False"
		if m.GetAnyCheckedCaller() {
			anyCheckedCaller = "True"
		}
		fmt.Fprintf(&py, "    %s: MethodPolicy(any_checked_caller=%s, requires=%s, carries=%s),\n", strconv.Quote(m.GetMethod()), anyCheckedCaller,
			pythonTuple(identifiers(m.GetRequires(), pythonNames)), pythonTuple(identifiers(m.GetCarries(), pythonNames)))
	}
	goCode.WriteString("} }\n")
	ts.WriteString("} as const;\n")
	py.WriteString("}\n")
	formatted, err := format.Source([]byte(goCode.String()))
	if err != nil {
		return nil, err
	}
	return map[string][]byte{"permissions_gen.go": formatted, "permissions_gen.ts": []byte(ts.String()), "permissions_gen.py": []byte(py.String())}, nil
}

// pythonTuple is a Python tuple of names; one name needs its trailing comma.
func pythonTuple(names []string) string {
	if len(names) == 1 {
		return "(" + names[0] + ",)"
	}
	return "(" + strings.Join(names, ", ") + ")"
}

// pythonPermissionsHeader opens the Python table: a permission is its kind and
// its action, kept apart, and a method's row is what the hop policy declares,
// in the field order of the authority client's own declaration type.
const pythonPermissionsHeader = `# Code generated by codefly generate permissions. DO NOT EDIT.
"""The permissions this service's published methods declare, and each method's row."""

from typing import NamedTuple


class Permission(NamedTuple):
    """One action on one resource kind."""

    kind: str
    action: str

    def __str__(self) -> str:
        return f"{self.kind}:{self.action}"


class MethodPolicy(NamedTuple):
    """What a method declares: every permission of requires, or any checked
    caller and nothing; carries is what a call may exercise beyond it."""

    any_checked_caller: bool
    requires: tuple[Permission, ...]
    carries: tuple[Permission, ...]


`
