package gitops

import (
	"context"
	"slices"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/runnables"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

// A module principal receives the union of its installing services' selections.
// Read the real delivered configuration, with the same profile, interpolation
// and credential rules as run/render. A module with no selected keys receives none.
func preparedSelection(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, services []*resources.Service, resolvers ...WorkspaceConfigurationResolver) (runnables.PreparedSelection, error) {
	keys := runnables.PreparedSelection{}
	resolve := selectionResolver(workspace, env, orchestration.SnapshotMode)
	if len(resolvers) > 0 && resolvers[0] != nil {
		resolve = resolvers[0]
	}
	for _, service := range services {
		identity, err := service.Identity()
		if err != nil {
			return nil, err
		}
		if env.Excludes(identity.Module, service.Name) || !slices.Contains(service.WorkspaceConfigurationDependencies, runnables.RunnableBindingsGroup) {
			continue
		}
		delivered, err := resolve(ctx, service)
		if err != nil {
			return nil, err
		}
		selected, err := runnables.PreparedSelectionFor(service, delivered)
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

// WorkspaceConfigurationResolver reuses the native flow's actual resolution;
// render callers use a value-free snapshot with its normal override guard.
type WorkspaceConfigurationResolver func(context.Context, *resources.Service) ([]*basev0.Configuration, error)

func selectionResolver(workspace *resources.Workspace, env *environments.Environment, mode orchestration.Mode) WorkspaceConfigurationResolver {
	var flow *orchestration.Flow
	return func(ctx context.Context, service *resources.Service) ([]*basev0.Configuration, error) {
		if flow == nil {
			var err error
			flow, err = orchestration.NewFlow(ctx, workspace, nil, nil, env, mode)
			if err != nil {
				return nil, err
			}
			flow.WithRuntimeContext(resources.RuntimeContextContainer)
			if err = flow.ConfigurationManager.Load(ctx, env.Runtime()); err != nil {
				return nil, err
			}
		}
		return flow.WorkspaceConfigurationsFor(ctx, service)
	}
}
