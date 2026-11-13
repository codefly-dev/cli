package gitops

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
)

// RenderStands answers whether module's working render for environment is
// one publish would deliver: rendered by a render that finished, of the
// composition as it is pinned now, baking in the configuration the
// environment provides now. It is the first thing publish holds a working
// render to (loadRenderedPublication), asked on its own so a whole-composition
// publish renders only when a render is owed; nil means a render would add
// nothing.
func RenderStands(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, module string) error {
	// Only the delivery path is read here: the repository is the publish's to
	// validate, a local file remote included.
	_, _, _, pathRoot, err := resolveGitops(workspace, env.Name, true)
	if err != nil {
		return err
	}
	request := &PublishRequest{Module: module, Environment: env.Name}
	_, err = loadRenderedPublication(ctx, workspace, request, env, moduleRenderDestination(workspace, env.Name, module), pathRoot)
	return err
}

// PublishOrder is the order a whole-composition publish delivers modules in:
// each module after the modules its services depend on, since a module's
// publication is held to the contracts the delivered producers expose; then
// the composition's hop, whose route table is baked in from the catalogs of
// every module behind it, so a module behind the hop is published before the
// hop that routes to it (hopRouteFeed); then the module hosting the
// environment's delivery API, whose publication withdraws what dropped
// modules last delivered. Modules with no order between them go by name, and
// a dependency cycle keeps its members in name order rather than refusing.
func PublishOrder(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, modules []string) ([]string, error) {
	host := ""
	if env.Host != nil {
		host, _, _ = env.Host.DeliveryEndpoint()
	}
	hop, err := derivedCompositionHop(ctx, workspace, env)
	if err != nil {
		return nil, fmt.Errorf("find the composition's hop to order the publication after the modules behind it: %w", err)
	}
	last := []string{}
	if hop != nil && hop.Module != host && slices.Contains(modules, hop.Module) {
		last = append(last, hop.Module)
	}
	if host != "" && slices.Contains(modules, host) {
		last = append(last, host)
	}
	dependsOn, err := moduleDependencies(ctx, workspace, env)
	if err != nil {
		return nil, err
	}
	var rest []string
	for _, module := range modules {
		if !slices.Contains(last, module) {
			rest = append(rest, module)
		}
	}
	return append(dependencyOrder(rest, dependsOn), last...), nil
}

// moduleDependencies maps each module to the other modules its services
// depend on, as the services declare them, less the services the environment
// withdraws.
func moduleDependencies(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) (map[string]map[string]bool, error) {
	refs, err := workspace.LoadServiceWithModules(ctx)
	if err != nil {
		return nil, fmt.Errorf("load the composition's services to order its publication: %w", err)
	}
	dependsOn := map[string]map[string]bool{}
	for _, ref := range refs {
		if env.Excludes(ref.Module, ref.Name) {
			continue
		}
		service, loadErr := workspace.LoadService(ctx, ref)
		if loadErr != nil {
			return nil, fmt.Errorf("load service %s/%s to order the publication: %w", ref.Module, ref.Name, loadErr)
		}
		for _, dependency := range service.ServiceDependencies {
			if dependency.Module == "" || dependency.Module == ref.Module {
				continue
			}
			if dependsOn[ref.Module] == nil {
				dependsOn[ref.Module] = map[string]bool{}
			}
			dependsOn[ref.Module][dependency.Module] = true
		}
	}
	return dependsOn, nil
}

// dependencyOrder sorts modules so each follows the modules of the set it
// depends on, ties and cycles by name.
func dependencyOrder(modules []string, dependsOn map[string]map[string]bool) []string {
	pending := slices.Clone(modules)
	sort.Strings(pending)
	var ordered []string
	placed := map[string]bool{}
	for len(pending) > 0 {
		next := slices.IndexFunc(pending, func(module string) bool {
			for dependency := range dependsOn[module] {
				if !placed[dependency] && slices.Contains(pending, dependency) {
					return false
				}
			}
			return true
		})
		if next < 0 {
			// A cycle: nothing left is free, so the first by name goes.
			next = 0
		}
		placed[pending[next]] = true
		ordered = append(ordered, pending[next])
		pending = slices.Delete(pending, next, next+1)
	}
	return ordered
}
