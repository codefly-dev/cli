package solutionrun

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/codefly-dev/core/resources"
)

// --- The surface a composed solution may reach directly ---
//
// A composed solution is reached through the host it is composed against, and
// the host decides what it may see. A declared service dependency is how a
// service asks for an address, and the render answers with that address: it is
// therefore the render that decides what a solution can reach, and it decides it
// from one declaration rather than from inference.
//
// The declaration is the environment's host surface. A solution may reach its
// own module's services, and the host surface the environment names. Everything
// else is refused by name — including every cross-module edge when the
// environment names no surface at all, because a composition that has not said
// what a guest may reach has not said that it may reach anything.
//
// Nothing here is derived from which modules look like hosts. An earlier shape
// admitted any service of a module that happened to hold the federation
// registrar, which made the permitted surface a consequence of where a
// configuration group was declared; two modules declaring it widened the surface
// of both, and a host service nobody had thought about was admitted by default.

// HostSurfaceEntry is one service, or one endpoint of one service, a solution
// may open a connection to directly.
type HostSurfaceEntry struct {
	// Module and Service name the permitted service.
	Module  string
	Service string
	// Endpoint, when set, permits only that endpoint of it. Empty permits the
	// service's endpoints.
	Endpoint string
}

// HostSurface is the whole declared surface. A nil or empty surface permits
// nothing, which is what makes an undeclared composition refuse rather than
// admit.
type HostSurface []HostSurfaceEntry

// permits reports whether the surface names this endpoint of this service.
func (surface HostSurface) permits(module, service, endpoint string) bool {
	for _, entry := range surface {
		if entry.Module != module || entry.Service != service {
			continue
		}
		if entry.Endpoint == "" || entry.Endpoint == endpoint {
			return true
		}
	}
	return false
}

// Describe renders the surface for a refusal, so an operator reads what IS
// permitted beside what was refused.
func (surface HostSurface) Describe() string {
	if len(surface) == 0 {
		return "none"
	}
	named := make([]string, 0, len(surface))
	for _, entry := range surface {
		name := resources.ServiceUnique(entry.Module, entry.Service)
		if entry.Endpoint != "" {
			name += "/" + entry.Endpoint
		}
		named = append(named, name)
	}
	sort.Strings(named)
	return strings.Join(named, ", ")
}

// SolutionBoundaries refuses a composition in which a solution declares a
// run-time path to something outside its own module and outside the surface the
// environment permits.
//
// A module is a solution because it ships a solution manifest, and for no other
// reason. An earlier shape also required the module to name a default runnable
// entry — a field a module render does not need — so a solution that omitted it
// was not checked at all. Every service the module declares is checked, not only
// an entry service: each one renders its own workload, and each one's
// dependencies become its own addresses.
//
// It walks the workspace rather than one module because the surface is declared
// for the environment the whole composition renders into. A module that cannot
// be loaded is skipped: the render loads every module it renders and fails on
// one it cannot, so a load failure here would report the wrong fault for a
// module this composition is not rendering.
//
// Inside a solution, the opposite holds. Its manifest and its services' declared
// dependencies are what this rule is judged against, so one that cannot be read
// refuses — a service whose edges cannot be read is not a service with no edges,
// and skipping it would turn an unreadable declaration into a pass.
//
// Build and schema edges are left alone: they are read by a toolchain, not
// opened by a workload, so they are no path in a cell.
func SolutionBoundaries(ctx context.Context, workspace *resources.Workspace, surface HostSurface) error {
	var refusals []string
	for _, ref := range workspace.Modules {
		mod, err := workspace.LoadModuleFromReference(ctx, ref)
		if err != nil {
			continue
		}
		solutionManifest, err := moduleManifest(mod)
		if err != nil {
			return fmt.Errorf("solution %s: %w", mod.Name, err)
		}
		if solutionManifest == nil {
			continue
		}
		// What the solution says it consumes, so a refusal can say the api is
		// already reached through the host rather than only that it is not
		// permitted. It never widens the surface.
		consumed := map[string]bool{}
		for _, api := range solutionManifest.ConsumedAPIs() {
			consumed[resources.ServiceUnique(api.Module, api.Service)] = true
		}
		for _, svcRef := range mod.ServiceReferences {
			service, err := workspace.LoadService(ctx, &resources.ServiceWithModule{Name: svcRef.Name, Module: mod.Name})
			if err != nil {
				// A service of a solution that cannot be read is not a service
				// with no edges. Its declarations are what this rule is judged
				// against, so not being able to read them refuses rather than
				// admits.
				return fmt.Errorf("cannot read the declarations of solution service %s: %w",
					resources.ServiceUnique(mod.Name, svcRef.Name), err)
			}
			refusals = append(refusals, solutionServiceRefusals(mod.Name, service, surface, consumed)...)
		}
	}
	if len(refusals) == 0 {
		return nil
	}
	sort.Strings(refusals)
	return fmt.Errorf(
		"a solution reaches another module through its host, and these declared dependencies are not that path:\n  %s\n"+
			"The environment permits a solution to reach: %s. Declare the api the solution consumes in its manifest and resolve its upstream through the host, "+
			"or name the host surface that serves it under the environment's solution-boundary.host-surface",
		strings.Join(refusals, "\n  "), surface.Describe())
}

// solutionServiceRefusals names every dependency of one solution service that
// crosses the boundary, one line per endpoint so an operator can act on each.
func solutionServiceRefusals(module string, service *resources.Service, surface HostSurface, consumed map[string]bool) []string {
	var refusals []string
	for _, dependency := range service.ServiceDependencies {
		if dependency == nil || !dependency.Kind.Participates(resources.StageRun) {
			continue
		}
		dependencyModule := dependency.Module
		if dependencyModule == "" {
			dependencyModule = module
		}
		if dependencyModule == module {
			continue
		}
		for _, endpoint := range dependencyEndpointNames(dependency) {
			if surface.permits(dependencyModule, dependency.Name, endpoint) {
				continue
			}
			target := resources.ServiceUnique(dependencyModule, dependency.Name)
			edge := fmt.Sprintf("%s depends on %s", resources.ServiceUnique(module, service.Name), target)
			if endpoint != "" {
				edge += "/" + endpoint
			}
			if consumed[target] {
				edge += ", whose api the solution already declares it consumes"
			}
			refusals = append(refusals, edge)
		}
	}
	return refusals
}

// dependencyEndpointNames are the endpoints a dependency names, or one empty
// name when it names none — a dependency on a whole service still has to be
// permitted, and an empty endpoint only matches a surface entry that permits the
// service rather than one endpoint of it.
func dependencyEndpointNames(dependency *resources.ServiceDependency) []string {
	if len(dependency.Endpoints) == 0 {
		return []string{""}
	}
	names := make([]string, 0, len(dependency.Endpoints))
	for _, endpoint := range dependency.Endpoints {
		names = append(names, endpoint.Name)
	}
	return names
}
