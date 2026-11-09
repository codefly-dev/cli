package gitops

import (
	"context"
	"slices"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/runnables"
	"github.com/codefly-dev/core/resources"
)

// A module principal receives the union of its installing services' selections.
// Read the real delivered configuration, with the same profile, interpolation
// and credential rules as run/render. An unselected installer preserves the
// module's existing catalog; a module with no selected keys receives none.
func preparedSelection(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, services []*resources.Service) (runnables.PreparedSelection, error) {
	keys := runnables.PreparedSelection{}
	for _, service := range services {
		identity, err := service.Identity()
		if err != nil {
			return nil, err
		}
		if env.Excludes(identity.Module, service.Name) || !slices.Contains(service.WorkspaceConfigurationDependencies, runnables.RunnableBindingsGroup) {
			continue
		}
		if !slices.Contains(service.WorkspaceConfigurationDependencies, runnables.RuntimeExecutionGroup) {
			return nil, nil
		}
		flow, err := orchestration.NewFlow(ctx, workspace, nil, nil, env, orchestration.SnapshotMode)
		if err != nil {
			return nil, err
		}
		flow.WithRuntimeContext(resources.RuntimeContextContainer)
		if err = flow.ConfigurationManager.Load(ctx, env.Runtime()); err != nil {
			return nil, err
		}
		delivered, err := flow.WorkspaceConfigurationsFor(ctx, service)
		if err != nil {
			return nil, err
		}
		selected, err := runnables.RuntimePreparedSelection(service, delivered)
		if err != nil {
			return nil, err
		}
		for key := range selected {
			keys[key] = true
		}
	}
	return keys, nil
}

func selectPreparedGrants(grants []preparedGrant, keys runnables.PreparedSelection) []preparedGrant {
	return slices.DeleteFunc(slices.Clone(grants), func(g preparedGrant) bool { return !keys.Includes(g.key) })
}
