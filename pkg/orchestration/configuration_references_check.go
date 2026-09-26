package orchestration

import (
	"context"
	"errors"
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
// configurations.CheckEndpointReferences). The same references fail again when a
// value is resolved: that is the last line of defence, this is the first.
//
// provided may be a read the caller already has; nil reads it here.
// excludedGroups names the groups a profile excludes, which the consumer never
// receives. excludedProducers names the services the caller removed from the
// plan (--exclude-dependency, a run profile): they are absent from dependencies
// like a service of another workspace, so without them the refusal would report
// "not a service of this plan" about a service the workspace does declare, and
// name neither the exclusion that caused it nor the way out.
func CheckConfigurationReferences(
	ctx context.Context, workspace *resources.Workspace, env *environments.Environment,
	provided *configurations.WorkspaceConfigurations,
	dependencies *architecture.ServiceDependencies, consumers []*resources.Service,
	excludedGroups map[string]bool, excludedProducers map[string]bool,
) error {
	if workspace == nil || env == nil || dependencies == nil || len(consumers) == 0 {
		return nil
	}
	if provided == nil {
		read, err := configurations.ReadWorkspaceConfigurations(ctx, workspace, env.Runtime())
		if err != nil {
			return fmt.Errorf("cannot read the workspace configurations to check their references: %w", err)
		}
		provided = read
	}
	err := configurations.CheckEndpointReferences(provided.Infos, consumers, excludedGroups, func(unique string) (*resources.Service, bool) {
		service, err := dependencies.ServiceFromUnique(unique)
		return service, err == nil
	})
	return withExcludedProducerReasons(err, excludedProducers)
}

// withExcludedProducerReasons restates every unresolved reference whose producer
// the caller itself excluded from the plan. Core cannot tell the two apart — an
// excluded service is simply not in the graph — so the reason it gives ("not a
// service of this plan") sends the reader looking for a missing composition. The
// real cause is the exclusion, and the fix is to exclude the group that
// references it as well, or to stop excluding the producer.
func withExcludedProducerReasons(err error, excludedProducers map[string]bool) error {
	if err == nil || len(excludedProducers) == 0 {
		return err
	}
	var unresolved *configurations.UnresolvedReferencesError
	if !errors.As(err, &unresolved) {
		return err
	}
	for i := range unresolved.References {
		reference := &unresolved.References[i]
		if !excludedProducers[reference.Producer] {
			continue
		}
		reference.Reason = fmt.Sprintf(
			"the producer is excluded from this run (--exclude-dependency or a run profile); exclude the %q workspace configuration too, or stop excluding %s",
			reference.Group, reference.Producer)
	}
	return err
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
	// One read serves both the ordering option and the check below.
	provided := readWorkspaceConfigurationsForReferences(ctx, workspace, env)
	var options []architecture.DependencyOption
	if option := configurationReferenceOptionFrom(provided); option != nil {
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
	if !standAlone {
		for _, root := range roots {
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
	}
	return CheckConfigurationReferences(ctx, workspace, env, provided, dependencies, consumers, nil, nil)
}

// checkConfigurationReferences runs the plan-time check for a flow whose run
// set is known: the origin and every service it will start or deploy. Only
// operations that resolve configurations are checked — building and syncing a
// service read none. Linting and compiling do: both create a Runner
// (Manager.Load) and RuntimeValidationPolicy schedules RuntimeInit for every
// service, which resolves the workspace configurations exactly as a run does.
//
// A service bound to a remote environment is left out: Runner.InitRemote only
// sets up networking and never resolves a workspace configuration, so a
// reference in a group it declares is never read and must not refuse the run.
func (flow *Flow) checkConfigurationReferences(ctx context.Context, required []string) error {
	switch flow.world.Mode {
	case RunMode, TestMode, DeployMode, SnapshotMode, LintMode, CompileMode:
	default:
		return nil
	}
	remote := make(map[string]bool, len(flow.remoteServices))
	for _, service := range flow.remoteServices {
		remote[service.Unique()] = true
	}
	uniques := append(slices.Clone(required), resources.WithUnique(flow.originService).Unique())
	slices.Sort(uniques)
	uniques = slices.Compact(uniques)
	consumers := make([]*resources.Service, 0, len(uniques))
	for _, unique := range uniques {
		if remote[unique] {
			continue
		}
		service, err := flow.world.Dependencies.ServiceFromUnique(unique)
		if err != nil {
			continue
		}
		consumers = append(consumers, service)
	}
	excludedProducers := make(map[string]bool, len(flow.excludedDependencyServices))
	for _, excluded := range flow.excludedDependencyServices {
		excludedProducers[excluded] = true
	}
	return CheckConfigurationReferences(ctx, flow.workspace, flow.world.Env, flow.providedWorkspaceConfigurations,
		flow.world.Dependencies, consumers, flow.world.excludedWorkspaceConfigurations, excludedProducers)
}
