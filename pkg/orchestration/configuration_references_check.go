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
// profile is the run profile the caller resolved; the groups it excludes are
// never received by a consumer (a zero profile excludes nothing), and core reads
// them from it rather than from a set assembled here. excludedProducers names the services the caller removed from the
// plan (--exclude-dependency, a run profile): they are absent from dependencies
// like a service of another workspace, so without them the refusal would report
// "not a service of this plan" about a service the workspace does declare, and
// name neither the exclusion that caused it nor the way out.
func CheckConfigurationReferences(
	ctx context.Context, workspace *resources.Workspace, env *environments.Environment,
	provided *configurations.WorkspaceConfigurations,
	dependencies *architecture.ServiceDependencies, consumers []*resources.Service,
	profile resources.RunProfile, excludedProducers map[string]bool, rootGroups []string,
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
	err := configurations.CheckEndpointReferences(provided.Infos,
		consumersWithEffectiveGroups(consumers, rootGroups, profile), profile, func(unique string) (*resources.Service, bool) {
			service, err := dependencies.ServiceFromUnique(unique)
			return service, err == nil
		})
	return withExcludedProducerReasons(err, excludedProducers)
}

// consumersWithEffectiveGroups restates each consumer with the group set it
// actually receives — the groups it declares unioned with the composition
// root's — because that is the set whose references get resolved
// (pkg/orchestration/workspace_configurations.go).
//
// Core's check reads WorkspaceConfigurationDependencies off the consumer, so
// handing it a copy carrying the effective set is how the plan gate comes to
// cover a composition-root group at all. Without it the gate validated the
// declared groups only: a root group's reference to a `private` endpoint, or to
// a producer the workspace does not have, passed the plan and then went
// unresolved at run time — a root group being, by definition, the one kind no
// service declares.
//
// The copies are shallow and the originals are never touched: they are the
// workspace's own service objects.
//
// The durable form of this is in core — let configurations.CheckEndpointReferences
// take the effective set — so that the gate and the resolution stop being two
// selections of one thing. Until then this is the same function reaching the
// same verdict over the same set.
func consumersWithEffectiveGroups(consumers []*resources.Service, rootGroups []string, profile resources.RunProfile) []*resources.Service {
	if len(rootGroups) == 0 {
		return consumers
	}
	excluded := make(map[string]bool, len(profile.ExcludeWorkspaceConfigurations))
	for _, group := range profile.ExcludeWorkspaceConfigurations {
		excluded[group] = true
	}
	out := make([]*resources.Service, 0, len(consumers))
	for _, consumer := range consumers {
		if consumer == nil {
			out = append(out, consumer)
			continue
		}
		effective := slices.Clone(consumer.WorkspaceConfigurationDependencies)
		declared := make(map[string]bool, len(effective))
		for _, group := range effective {
			declared[group] = true
		}
		for _, group := range rootGroups {
			if declared[group] || excluded[group] {
				continue
			}
			declared[group] = true
			effective = append(effective, group)
		}
		restated := *consumer
		restated.WorkspaceConfigurationDependencies = effective
		out = append(out, &restated)
	}
	return out
}

// CompositionRootGroupNames names the composition root's own groups as a read
// reports them: every group no composed module contributed.
//
// It is the plan-time counterpart of the loader capability the resolution uses
// (configurations.Loader.CompositionRootWorkspaceConfigurationNames), for the
// callers that have a read but no loader. One difference, stated rather than
// hidden: a loader also treats an invocation-scoped override (--set) as
// composition-root even on a name a composed module provides, and a plain read
// cannot see those. The flow's own gate passes the loader's set, so the
// narrower derivation here only ever checks less than that gate, never
// something different.
func CompositionRootGroupNames(provided *configurations.WorkspaceConfigurations) []string {
	if provided == nil {
		return nil
	}
	var out []string
	for _, info := range provided.Infos {
		if _, composed := provided.ComposedBy[info.GetName()]; composed {
			continue
		}
		out = append(out, info.GetName())
	}
	slices.Sort(out)
	return slices.Compact(out)
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
	return CheckConfigurationReferences(ctx, workspace, env, provided, dependencies, consumers,
		resources.RunProfile{}, nil, CompositionRootGroupNames(provided))
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
		flow.world.Dependencies, consumers, flow.runProfile, excludedProducers,
		flow.world.compositionRootWorkspaceConfigurationGroups())
}
