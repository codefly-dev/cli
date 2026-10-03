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
	// Two calls, because the two halves of the effective set must be judged
	// against different producer sets.
	//
	// A group the consumer DECLARES is judged against the plan's graph, exactly
	// as it always was: a producer the caller excluded is absent from that graph,
	// and withExcludedProducerReasons restates the refusal to name the exclusion
	// rather than send the reader hunting for a missing composition.
	//
	// A group only the composition root provides is judged against the WHOLE
	// workspace. It has to be: a root group reaches every service of every run,
	// including a module-closure run (`--module`) whose graph is a slice of the
	// workspace, and judging it against that slice would refuse every such run
	// whose root group names a producer outside the closure — reported, wrongly,
	// as "not a service of this workspace". Whether a producer is in THIS run is
	// not a plan-time question; this gate answers only whether the reference is
	// legal at all.
	declaredProblems := configurations.CheckEndpointReferences(provided.Infos, consumers, profile,
		func(unique string) (*resources.Service, bool) {
			service, err := dependencies.ServiceFromUnique(unique)
			return service, err == nil
		})
	err := withExcludedProducerReasons(declaredProblems, excludedProducers)
	rootConsumers := consumersWithRootGroupsOnly(consumers, rootGroups, profile)
	if len(rootConsumers) == 0 {
		return err
	}
	lookup, lookupErr := workspaceProducerLookup(ctx, workspace)
	if lookupErr != nil {
		return errors.Join(err, lookupErr)
	}
	rootProblems := configurations.CheckEndpointReferences(provided.Infos, rootConsumers, profile, lookup)
	return mergeUnresolvedReferences(err, rootProblems)
}

// mergeUnresolvedReferences returns the two checks' findings as ONE
// UnresolvedReferencesError.
//
// errors.Join would not do: every caller reaches the findings through
// errors.As, which stops at the first match in a joined tree, so the second
// check's references would be carried in the error and read by nobody — the
// `codefly doctor` listing, the render's refusal and every test would silently
// report half of what was found. Anything that is not an
// UnresolvedReferencesError (an unreadable workspace, say) is joined as a
// separate cause, because it is not a finding about a reference.
func mergeUnresolvedReferences(first, second error) error {
	var left, right *configurations.UnresolvedReferencesError
	switch {
	case first == nil:
		return second
	case second == nil:
		return first
	case !errors.As(first, &left) || !errors.As(second, &right):
		return errors.Join(first, second)
	}
	merged := &configurations.UnresolvedReferencesError{
		References: make([]configurations.UnresolvedReference, 0, len(left.References)+len(right.References)),
	}
	seen := make(map[configurations.UnresolvedReference]bool, len(left.References)+len(right.References))
	for _, reference := range slices.Concat(left.References, right.References) {
		if seen[reference] {
			continue
		}
		seen[reference] = true
		merged.References = append(merged.References, reference)
	}
	return merged
}

// consumersWithRootGroupsOnly restates each consumer carrying ONLY the groups
// the composition root provides that it did not declare — the half of the
// effective set the plan gate did not use to cover.
//
// Core's check reads WorkspaceConfigurationDependencies off the consumer, so
// handing it a copy carrying those names is how the gate comes to see a
// composition-root group at all. Without it the gate validated declared groups
// only: a root group's reference to a `private` endpoint, or to a producer the
// workspace does not have, passed the plan and then failed (or was quietly
// dropped) per service at run time — a root group being, by definition, the one
// kind no service declares.
//
// The copies are shallow and the originals are never touched: they are the
// workspace's own service objects. A consumer with no root-only group yields
// nothing, so the second check is skipped entirely for a composition that has
// none.
//
// The durable form of this is in core — let configurations.CheckEndpointReferences
// take the effective set — so that the gate and the resolution stop being two
// selections of one thing.
func consumersWithRootGroupsOnly(consumers []*resources.Service, rootGroups []string, profile resources.RunProfile) []*resources.Service {
	if len(rootGroups) == 0 {
		return nil
	}
	// The profile's exclusions, so a consumer whose only root-only groups are
	// excluded yields nothing and the second check is skipped for it. Core
	// applies the same exclusion inside CheckEndpointReferences, which is handed
	// the profile, so removing this changes no verdict — it is a narrowing of
	// what gets asked, not a guard. (Layer-5 round four reported the mutation as
	// surviving; it survives because it is observationally equivalent, which is
	// pinned end-to-end by
	// TestARunProfileExclusionAlsoExcludesARootGroupFromThePlanGate.)
	excluded := make(map[string]bool, len(profile.ExcludeWorkspaceConfigurations))
	for _, group := range profile.ExcludeWorkspaceConfigurations {
		excluded[group] = true
	}
	out := make([]*resources.Service, 0, len(consumers))
	for _, consumer := range consumers {
		if consumer == nil {
			continue
		}
		declared := make(map[string]bool, len(consumer.WorkspaceConfigurationDependencies))
		for _, group := range consumer.WorkspaceConfigurationDependencies {
			declared[group] = true
		}
		var rootOnly []string
		for _, group := range rootGroups {
			if declared[group] || excluded[group] {
				continue
			}
			rootOnly = append(rootOnly, group)
		}
		if len(rootOnly) == 0 {
			continue
		}
		restated := *consumer
		restated.WorkspaceConfigurationDependencies = rootOnly
		out = append(out, &restated)
	}
	return out
}

// workspaceProducerLookup resolves <module>/<service> over the whole workspace.
// It fails closed: an unreadable workspace means no reference can be checked,
// and a reference delivered unchecked is how a private endpoint's address
// reaches a service that may not see it.
func workspaceProducerLookup(ctx context.Context, workspace *resources.Workspace) (configurations.ProducerLookup, error) {
	services, err := workspace.LoadServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot read this workspace's services, so the ${endpoint:…} references its composition-root configurations carry cannot be checked against the producers they name: %w", err)
	}
	byUnique := make(map[string]*resources.Service, len(services))
	for _, service := range services {
		identity, err := service.Identity()
		if err != nil {
			continue
		}
		byUnique[identity.Unique()] = service
	}
	return func(unique string) (*resources.Service, bool) {
		service, ok := byUnique[unique]
		return service, ok
	}, nil
}

// WorkspaceConfigurationsForChecking reads the workspace configurations the way
// the run will actually resolve them, and names the composition root's groups
// among them.
//
// It loads a configuration local reader rather than calling
// configurations.ReadWorkspaceConfigurations, and the difference is the point:
// the reader applies the **invocation-scoped overrides** (carried in
// CODEFLY__WORKSPACE_CONFIGURATION_OVERRIDES, core's SDK-to-CLI carrier, which
// an integration harness sets — not `--set`, which is a per-service runtime
// environment override the CLI applies elsewhere) and derives its
// composition-root names from the result. Core applies them inside the loader
// (applyWorkspaceConfigurationOverrides, unexported), so loading one is the only
// way to see them from here.
//
// Both halves matter, and a plain read gets both wrong:
//
//   - A reference supplied by an override is invisible to a plain read, so a
//     typo'd producer in an overridden value escaped the gate entirely and the
//     resolution then dropped the value. Whoever supplied the override got no
//     error from the thing they had broken.
//   - An override makes its group composition-root even on a name a composed
//     module provides (the run itself is supplying the value, so it reaches every
//     service), which a plain read cannot know — so the group's references went
//     unchecked for every service that did not declare it.
//
// It is also the one place the composition-root names are derived, which is why
// there is no longer a CLI-side "not ComposedBy ⇒ root" rule: this asks core's
// own loader.
//
// A reader that cannot load returns its error, and every caller fails closed on
// it. An earlier revision logged it at Debug and answered "not invocation-aware",
// which let the caller fall back to a plain disk read — and the fallback carried
// NO composition-root names, so a load failure in one group silently stopped
// every root group's references being checked at all. `codefly doctor` then
// printed "every endpoint reference resolves, in the groups each service
// declares and in the composition root's own" over a workspace holding a
// typo'd producer in a root group: an unrelated unsupplied ${profile} value was
// enough to reach it. Failing closed costs nothing in a run or a render, where
// the same Load runs again a moment later and fails the operation anyway; what it
// buys is that a check never reports a pass it did not perform. The doctor turns
// the error into "not checked: <reason>", which is its contract: nothing here
// returns silently.
func WorkspaceConfigurationsForChecking(
	ctx context.Context, workspace *resources.Workspace, env *environments.Environment,
) (*configurations.WorkspaceConfigurations, []string, error) {
	if workspace == nil || env == nil {
		return nil, nil, nil
	}
	reader, err := configurations.NewConfigurationLocalReader(ctx, workspace)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot build a configuration reader, so the workspace configurations this invocation resolves cannot be checked: %w", err)
	}
	if err := reader.Load(ctx, env.Runtime()); err != nil {
		return nil, nil, fmt.Errorf("cannot load the workspace configurations this invocation resolves, so their ${endpoint:…} references cannot be checked: %w", err)
	}
	return &configurations.WorkspaceConfigurations{
		Infos: workspaceConfigurationInfos(reader.Configurations()),
	}, reader.CompositionRootWorkspaceConfigurationNames(), nil
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
	// One read serves both the ordering option and the check below: the
	// configurations as this invocation will resolve them, overrides included,
	// so a reference supplied through the override carrier is checked like any
	// other. A read that fails refuses the plan rather than falling back to a
	// narrower one — the fallback checked no composition-root group at all.
	provided, rootGroups, err := WorkspaceConfigurationsForChecking(ctx, workspace, env)
	if err != nil {
		return err
	}
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
		resources.RunProfile{}, nil, rootGroups)
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
	// The configurations this run will actually resolve, overrides included, and
	// the root groups among them. The loader the flow itself registers has not
	// Loaded yet at this point in the lifecycle (this gate runs inside
	// InitManagers; Load follows), which is why a reader is loaded here rather
	// than read off the world — and why the names cannot come from
	// world.compositionRootGroups, which would be empty.
	//
	// A read that fails refuses the operation here too. The fallback that used
	// to stand here (flow.providedWorkspaceConfigurations plus
	// world.compositionRootWorkspaceConfigurationGroups) was worse than no
	// check: the first cannot see an invocation override, and the second is
	// empty at this point in the lifecycle, so a load failure anywhere in the
	// configurations turned the gate into a declared-groups-only check without
	// saying so.
	provided, rootGroups, err := WorkspaceConfigurationsForChecking(ctx, flow.workspace, flow.world.Env)
	if err != nil {
		return err
	}
	return CheckConfigurationReferences(ctx, flow.workspace, flow.world.Env, provided,
		flow.world.Dependencies, consumers, flow.runProfile, excludedProducers, rootGroups)
}
