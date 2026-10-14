package orchestration

import (
	"context"
	"slices"

	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/resources"
)

// includeConsumedRoots feeds declared API producers into the existing multi-root
// run. Core still owns module closure, configuration edges and stage ordering.
// Recompute from authored roots on reload so a withdrawn solution cannot keep
// its formerly consumed services alive.
func (flow *Flow) includeConsumedRoots(ctx context.Context) error {
	if flow.world.Mode != RunMode || flow.standAlone {
		return nil
	}
	roots := append([]string{resources.WithUnique(flow.originService).Unique()}, flow.coRoots...)
	authored := len(roots)
	_, options := flow.dependencyGraphOptions()
	for {
		var seeds []string
		if len(flow.moduleSeeds) > 0 {
			for _, root := range roots {
				identity, err := resources.ParseServiceWithOptionalModule(root)
				if err != nil {
					return err
				}
				if !slices.Contains(seeds, identity.Module) {
					seeds = append(seeds, identity.Module)
				}
			}
		}
		workspace, err := runModuleClosure(ctx, flow.workspace, seeds, flow.providedWorkspaceConfigurations)
		if err != nil {
			return err
		}
		graph, err := architecture.NewServiceDependencies(ctx, workspace, options...)
		if err != nil {
			return err
		}
		graph, err = graph.ForStage(resources.StageRun)
		if err != nil {
			return err
		}
		selected := slices.Clone(roots)
		for _, root := range roots {
			order, err := graph.OrderTo(ctx, root)
			if err != nil {
				return err
			}
			for _, service := range order {
				if !slices.Contains(selected, service.Unique) {
					selected = append(selected, service.Unique)
				}
			}
		}
		consumed, err := solutionrun.SelectedConsumedEndpoints(ctx, flow.workspace, selected)
		if err != nil {
			return err
		}
		before := len(roots)
		for _, endpoint := range consumed {
			root := resources.ServiceUnique(endpoint.Module, endpoint.Service)
			if !slices.Contains(roots, root) && !slices.Contains(flow.excludedDependencyServices, root) {
				roots = append(roots, root)
			}
		}
		if len(roots) == before {
			flow.consumedAPIs = consumed
			flow.consumedRoots = slices.Clone(roots[authored:])
			flow.moduleSeeds = seeds
			flow.graphWorkspace = workspace
			flow.world.Dependencies = graph
			if flow.SharedState != nil {
				flow.SharedState.SetDependencies(graph)
			}
			return nil
		}
	}
}
