package orchestration

import (
	"context"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// A workspace's module list answers where a module named X comes from. Which
// modules a run needs is a different question, and answering it from that same
// list makes every run carry the whole composition: a solution spanning two
// modules only runs once someone also writes down the modules it reaches, and a
// module pinned for a different solution joins the graph regardless.
//
// The run already states the answer — its roots, plus the dependencies each
// service declares — so the graph is derived from those and resolved against
// the pin set instead.

// FlowOption configures a flow at construction. Everything settable afterwards
// is a flow.WithX setter; this is for what NewFlow needs before it builds the
// dependency graph.
type FlowOption func(*flowOptions)

type flowOptions struct {
	moduleClosureSeeds []string
}

// WithRunModuleClosure derives the flow's dependency graph from the modules its
// roots reach at run stage, rather than from every module the workspace pins.
// Seeds are the module names the run starts from: the origin's, and one per
// co-root.
func WithRunModuleClosure(seeds ...string) FlowOption {
	return func(options *flowOptions) {
		options.moduleClosureSeeds = seeds
	}
}

// runModuleClosure narrows workspace to the modules seeds reach, and refuses the
// run when that closure breaks the endpoint visibility rules. The refusal shares
// core's implementation with the workspace-wide pass, so a graph that runs is a
// graph that validates — a violation outside the closure belongs to the runs
// that do reach it, and to the workspace-wide pass, not to this one.
//
// The narrowed workspace serves the dependency graph alone. Workspace
// configurations and run profiles are declared by the composition and stay
// resolved against all of it.
func runModuleClosure(ctx context.Context, workspace *resources.Workspace, seeds []string) (*resources.Workspace, error) {
	w := wool.Get(ctx).In("orchestration.runModuleClosure", wool.NameField(workspace.Name))
	if len(seeds) == 0 {
		return workspace, nil
	}
	closure, err := workspace.ResolveModuleClosure(ctx, resources.StageRun, seeds)
	if err != nil {
		return nil, w.Wrap(err)
	}
	if err = closure.ValidateServiceDependencies(ctx); err != nil {
		return nil, w.Wrap(err)
	}
	scoped := workspace.Clone()
	scoped.Modules = nil
	for _, ref := range workspace.Modules {
		if closure.Contains(ref.Name) {
			scoped.Modules = append(scoped.Modules, ref)
		}
	}
	return scoped, nil
}
