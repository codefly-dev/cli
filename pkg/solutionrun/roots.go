package solutionrun

import (
	"context"

	"github.com/codefly-dev/core/resources"
)

// SolutionRoots returns, among the modules declaring a service-entry, the ones
// no other such module depends on. A host composed beside a solution declares an
// entry of its own — the module a browser reaches first — but the solution's
// entry depends on it, through its services' dependencies or its manifest's
// api.consumes, which makes the host a dependency of the solution and never the
// root of the run. Only an entry nothing else in the set needs can be what the
// whole composition hangs off.
//
// Dependencies are followed transitively through every service the workspace
// can load, so an entry that reaches another only through a module with no
// entry of its own is still not a root. A module or service that fails to load
// contributes no edges: a module this run cannot load is not one it can start
// either, and treating it as a root would only move the failure later.
func SolutionRoots(ctx context.Context, workspace *resources.Workspace, entries []*resources.Module) []*resources.Module {
	depended := make(map[string]bool)
	for _, entry := range entries {
		for module := range dependedModules(ctx, workspace, entry) {
			depended[module] = true
		}
	}
	roots := make([]*resources.Module, 0, len(entries))
	for _, entry := range entries {
		if !depended[entry.Name] {
			roots = append(roots, entry)
		}
	}
	return roots
}

// dependedModules returns the modules a module's services reach through their
// service-dependencies, transitively, plus the modules its solution manifest
// consumes — every module it needs running to serve — excluding itself.
func dependedModules(ctx context.Context, workspace *resources.Workspace, module *resources.Module) map[string]bool {
	reached := make(map[string]bool)
	visited := make(map[string]bool)
	queue := make([]*resources.ServiceWithModule, 0, len(module.ServiceReferences))
	for _, ref := range module.ServiceReferences {
		queue = append(queue, &resources.ServiceWithModule{Name: ref.Name, Module: module.Name})
	}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		unique := resources.ServiceUnique(next.Module, next.Name)
		if visited[unique] {
			continue
		}
		visited[unique] = true
		service, err := workspace.LoadService(ctx, next)
		if err != nil {
			continue
		}
		for _, dependency := range service.ServiceDependencies {
			dependencyModule := dependency.Module
			if dependencyModule == "" {
				dependencyModule = next.Module
			}
			reached[dependencyModule] = true
			queue = append(queue, &resources.ServiceWithModule{Name: dependency.Name, Module: dependencyModule})
		}
	}
	// The manifest's api.consumes are consumed at run time through the gateway
	// rather than declared as service-dependencies, so they are edges the
	// service manifests do not carry. Read leniently as for a run: a manifest
	// this run would refuse later is not what decides the root now.
	if solutionManifest, err := moduleManifest(module); err == nil && solutionManifest != nil {
		for _, consumed := range solutionManifest.ConsumedAPIs() {
			reached[consumed.Module] = true
		}
	}
	delete(reached, module.Name)
	return reached
}
