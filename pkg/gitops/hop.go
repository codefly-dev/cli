package gitops

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"

	"github.com/codefly-dev/authority/libraries/hop/go/routing"
)

// A MODULE SERVICE IS REACHABLE ONLY FROM ITS HOP.
//
// The hop is the proxy a composition stands in front of its modules: every
// consumed module is reached THROUGH it, and the hop decides every call
// against the policy table the authority holds for that listener. The point of
// standing one is that a module's own code never has to get the check right —
// so a module's code must have no way to RECEIVE traffic except through the
// hop. A single mesh rule admitting a caller straight to a module's listener
// undoes the hop for every call that caller makes, and nothing in the module
// would say so.
//
// The composition states both halves already, in one place each, and this
// file is the one derivation that joins them:
//
//   - WHICH MODULES ARE BEHIND A HOP: the `hop` workspace-configuration
//     group's MODULES value, the same list orchestration reads to generate the
//     hop's route table (`generatedHopRoutes`). A module named there publishes
//     its API through the hop.
//   - WHICH SERVICE IS THE HOP: the service declaring the `hop-routes`
//     workspace-configuration group, the one reader of that generated table.
//     Nothing else may declare it (the group is generated, never authored), so
//     the declaration IS the identity of the hop.
//   - WHICH LISTENERS THE HOP REACHES: the generated table itself, `hop-routes`
//     ROUTES, one entry per published listener of every fronted module. The
//     hop is admitted to exactly those, which is narrower and more exact than
//     anything a dependency graph could say: core's derived allow-list is at
//     MODULE granularity, and it is derived from service dependencies, which
//     the hop does not declare — it is handed addresses through a
//     configuration group. So the route table is the only statement of the
//     hop's reach, and it is the one the hop itself routes by.
//
// None of the three is inferred: a composition that wants a hop says so, and a
// composition that names modules for a hop no service stands is refused by
// name rather than rendered with a reach nothing checks.
//
// What this derivation is NOT. It is not a dependency graph. A module endpoint
// does not become hop-reachable because something declares an edge to it; the
// edge is exactly what this file exists to refuse. And it says nothing about
// authorization: whether a call is permitted is the authority's per-call
// verdict, carried in a Work Context. This is L4 reachability only.

const (
	// hopRoutesGroup is the generated workspace-configuration group the
	// composition's hop reads its route table from. Declaring it is what makes
	// a service the hop (orchestration owns the generation; this package only
	// reads the declaration).
	hopRoutesGroup = "hop-routes"
	// hopRoutesKey is the one value of the generated group: the hop's route
	// table, naming every listener it reaches.
	hopRoutesKey = "ROUTES"
	// hopGroup is the authored group a composition states its hop's inputs in,
	// and hopModulesKey the one value this package reads from it: the modules
	// published through the hop, comma-separated.
	hopGroup      = "hop"
	hopModulesKey = "MODULES"
)

// compositionHop is the composition's hop and the modules it fronts, derived
// once per render by derivedCompositionHop. A nil hop is a composition that
// stands none: nothing is narrowed and nothing is refused.
type compositionHop struct {
	// Module and Service name the hop service.
	Module  string
	Service string
	// Fronted are the modules published through it, sorted and each once.
	Fronted []string
	// Reached is every listener the hop's generated route table names, keyed
	// `<module>/<service>/<endpoint>`: the endpoints the hop's principal is
	// admitted to, and no others.
	Reached map[string]bool
}

// reaches reports whether the hop's route table routes one endpoint.
func (hop *compositionHop) reaches(module, service, endpoint string) bool {
	if hop == nil {
		return false
	}
	return hop.Reached[resources.ServiceUnique(module, service)+"/"+endpoint]
}

// unique is the `<module>/<service>` of the hop.
func (hop *compositionHop) unique() string {
	if hop == nil {
		return ""
	}
	return resources.ServiceUnique(hop.Module, hop.Service)
}

// fronts reports whether a module's services are reached through this hop. The
// hop's own module is never behind itself: the hop is reached directly, which
// is the whole point of it.
func (hop *compositionHop) fronts(module string) bool {
	if hop == nil || module == hop.Module {
		return false
	}
	return slices.Contains(hop.Fronted, module)
}

// derivedCompositionHop derives the composition's hop for one environment. It
// returns nil for a composition that names no module for a hop — there is
// nothing to be behind, so nothing is narrowed and nothing is refused — and
// refuses, by name, a composition whose statements disagree: modules named
// with no service standing the hop, two services standing one (the module
// endpoints would have two admitted principals with nothing saying which
// checks the call), or the hop's own module named behind itself.
func derivedCompositionHop(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) (*compositionHop, error) {
	listed, reached, err := hopConfiguration(ctx, workspace, env)
	if err != nil {
		return nil, err
	}
	if len(listed) == 0 {
		return nil, nil
	}
	refs, err := workspace.LoadServiceWithModules(ctx)
	if err != nil {
		return nil, err
	}
	var standing []string
	hop := &compositionHop{Fronted: listed, Reached: reached}
	for _, ref := range refs {
		service, loadErr := workspace.LoadService(ctx, ref)
		if loadErr != nil {
			return nil, fmt.Errorf("load service %s/%s to find the composition's hop: %w", ref.Module, ref.Name, loadErr)
		}
		if !slices.Contains(service.WorkspaceConfigurationDependencies, hopRoutesGroup) {
			continue
		}
		standing = append(standing, resources.ServiceUnique(ref.Module, ref.Name))
		hop.Module, hop.Service = ref.Module, ref.Name
	}
	sort.Strings(standing)
	switch len(standing) {
	case 0:
		return nil, fmt.Errorf("%s/%s names modules %s, but no service of the composition declares %s, so nothing stands the hop those modules are published through: declare the group on the hop service, or stop naming the modules",
			hopGroup, hopModulesKey, strings.Join(listed, ", "), hopRoutesGroup)
	case 1:
	default:
		return nil, fmt.Errorf("%s and %s both declare %s, so a module endpoint behind the hop has two admitted principals and nothing says which one checks the call; one service stands the composition's hop",
			standing[0], standing[1], hopRoutesGroup)
	}
	// A module the composition does not carry is refused where the route
	// table is generated, by name and before this runs, so nothing is
	// restated here.
	if slices.Contains(listed, hop.Module) {
		return nil, fmt.Errorf("%s/%s names %s, the hop's own module: the hop is reached directly — that is what standing one means — so it cannot be published through itself",
			hopGroup, hopModulesKey, hop.Module)
	}
	return hop, nil
}

// hopConfiguration reads, in ONE load of the environment's configurations, the
// two values the hop is derived from: the `hop` group's MODULES — the same
// source orchestration generates the route table from — and the generated
// `hop-routes` ROUTES table itself.
//
// The modules come back sorted, each once; an empty or duplicate entry is
// refused where the table is generated, so a blank one is dropped here rather
// than named twice. The table is decoded by the hop's own routing library, the
// same decoder the hop runs, so what the render admits is what the hop routes.
func hopConfiguration(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) ([]string, map[string]bool, error) {
	reader, err := orchestration.NewWorkspaceConfigurationReader(ctx, workspace)
	if err != nil {
		return nil, nil, err
	}
	if err = reader.Load(ctx, env.Runtime()); err != nil {
		return nil, nil, fmt.Errorf("read the configurations of environment %s for the composition's hop: %w", env.Name, err)
	}
	var listed, table string
	for _, conf := range reader.Configurations() {
		if conf.GetOrigin() != resources.ConfigurationWorkspace {
			continue
		}
		for _, info := range conf.GetInfos() {
			for _, value := range info.GetConfigurationValues() {
				switch {
				case info.GetName() == hopGroup && value.GetKey() == hopModulesKey:
					listed = value.GetValue()
				case info.GetName() == hopRoutesGroup && value.GetKey() == hopRoutesKey:
					table = value.GetValue()
				}
			}
		}
	}
	var modules []string
	for _, name := range strings.Split(listed, ",") {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			modules = append(modules, trimmed)
		}
	}
	slices.Sort(modules)
	reached := map[string]bool{}
	if strings.TrimSpace(table) != "" {
		routes, decodeErr := routing.DecodeRouting(table)
		if decodeErr != nil {
			return nil, nil, fmt.Errorf("decode the hop's route table of environment %s: %w", env.Name, decodeErr)
		}
		for i := range routes.Endpoints {
			route := &routes.Endpoints[i]
			reached[resources.ServiceUnique(route.Audience, route.Service)+"/"+route.Endpoint] = true
		}
	}
	return slices.Compact(modules), reached, nil
}
