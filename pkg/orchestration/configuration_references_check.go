package orchestration

import (
	"context"
	"fmt"
	"slices"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/resources"
)

// CheckConfigurationReferences refuses a plan whose workspace configurations
// carry an ${endpoint:…} reference that cannot resolve, before anything is
// built, pushed or started. It checks every group each consumer declares, as
// the environment provides it, against the producers dependencies holds, and
// lists every unresolved reference in one error (core's
// configurations.CheckEndpointReferences). excluded names the groups a profile
// excludes. The same references fail again when a value is resolved: that is
// the last line of defence, this is the first.
func CheckConfigurationReferences(
	ctx context.Context, workspace *resources.Workspace, env *environments.Environment,
	dependencies *architecture.ServiceDependencies, consumers []*resources.Service, excluded map[string]bool,
) error {
	if workspace == nil || env == nil || dependencies == nil || len(consumers) == 0 {
		return nil
	}
	provided, err := configurations.ReadWorkspaceConfigurations(ctx, workspace, env.Runtime())
	if err != nil {
		return fmt.Errorf("cannot read the workspace configurations to check their references: %w", err)
	}
	return configurations.CheckEndpointReferences(provided.Infos, consumers, excluded, func(unique string) (*resources.Service, bool) {
		service, err := dependencies.ServiceFromUnique(unique)
		return service, err == nil
	})
}

// PlanConfigurationReferences is CheckConfigurationReferences for a plan that
// has no flow yet — a render, a dev deploy, a CI gate: the consumers are roots
// and, unless standAlone, every service they depend on, over the same graph a
// flow of those roots builds (ordered by the configuration references
// themselves).
func PlanConfigurationReferences(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, roots []*resources.Service, standAlone bool) error {
	if workspace == nil || env == nil || len(roots) == 0 {
		return nil
	}
	var options []architecture.DependencyOption
	if option := configurationReferenceOption(ctx, workspace, env); option != nil {
		options = append(options, option)
	}
	dependencies, err := architecture.NewServiceDependencies(ctx, workspace, options...)
	if err != nil {
		return err
	}
	// The roots are checked as given — a dev deploy's root is its source
	// checkout's manifest, not the workspace's — and their dependencies as the
	// graph holds them.
	consumers := slices.Clone(roots)
	seen := make(map[string]bool)
	for _, root := range roots {
		seen[resources.WithUnique(root).Unique()] = true
	}
	for _, root := range roots {
		if standAlone {
			break
		}
		order, err := dependencies.OrderTo(ctx, resources.WithUnique(root).Unique())
		if err != nil {
			return err
		}
		for _, dependency := range order {
			if seen[dependency.Unique] {
				continue
			}
			seen[dependency.Unique] = true
			if service, err := dependencies.ServiceFromUnique(dependency.Unique); err == nil {
				consumers = append(consumers, service)
			}
		}
	}
	return CheckConfigurationReferences(ctx, workspace, env, dependencies, consumers, nil)
}

// checkConfigurationReferences runs the plan-time check for a flow whose run
// set is known: the origin and every service it will start or deploy. Only
// operations that resolve configurations — run, test, deploy and render — are
// checked; building, linting or syncing a service reads none.
func (flow *Flow) checkConfigurationReferences(ctx context.Context, required []string) error {
	switch flow.world.Mode {
	case RunMode, TestMode, DeployMode, SnapshotMode:
	default:
		return nil
	}
	uniques := append(slices.Clone(required), resources.WithUnique(flow.originService).Unique())
	slices.Sort(uniques)
	uniques = slices.Compact(uniques)
	consumers := make([]*resources.Service, 0, len(uniques))
	for _, unique := range uniques {
		service, err := flow.world.Dependencies.ServiceFromUnique(unique)
		if err != nil {
			continue
		}
		consumers = append(consumers, service)
	}
	return CheckConfigurationReferences(ctx, flow.workspace, flow.world.Env, flow.world.Dependencies, consumers, flow.world.excludedWorkspaceConfigurations)
}
