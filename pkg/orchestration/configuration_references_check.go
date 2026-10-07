package orchestration

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
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
		// Refused rather than read here. Every caller now hands in the
		// configurations this invocation will RESOLVE
		// (WorkspaceConfigurationsForChecking), and a plain read taken at this
		// point would be a second, narrower source: blind to invocation
		// overrides and carrying no composition-root names, which is exactly
		// the fallback that let a load failure turn this gate into a
		// declared-groups-only check without saying so.
		return fmt.Errorf("cannot check workspace configuration references: no configurations were supplied to check, and this gate must not read a second, narrower source of its own (pass the result of WorkspaceConfigurationsForChecking)")
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
	// checkedInfos is the set core counts reference POSITIONS over, and the
	// only set a position may be indexed back into. The two uses below take
	// this one local rather than reaching for provided.Infos separately: a
	// position is an index into one value's reference list, so correlating it
	// against a DIFFERENT set — a credential-narrowed one, say — indexes a
	// different list and names the wrong producer. Binding them to one name is
	// what stops that being reintroduced by an edit.
	checkedInfos := provided.Infos
	declaredProblems := configurations.CheckEndpointReferences(checkedInfos, consumers, profile,
		func(unique string) (*resources.Service, bool) {
			service, err := dependencies.ServiceFromUnique(unique)
			return service, err == nil
		})
	err := withExcludedProducerReasons(declaredProblems, excludedProducers, checkedInfos)
	lookup, lookupErr := workspaceProducerLookup(ctx, workspace)
	if lookupErr != nil {
		return errors.Join(err, lookupErr)
	}
	rootConsumers := consumersWithRootGroupsOnly(consumers, rootGroups, profile)
	if len(rootConsumers) == 0 {
		if err != nil {
			return err
		}
		return checkPlanReferencesPerConsumer(ctx, consumers, provided.Infos, rootGroups, profile, lookup)
	}
	// Without the credentials those consumers will never receive. A root group's
	// credentials reach only the services that declare it, and a value nobody
	// receives must not refuse their plan: a credential referencing an endpoint
	// private to the producer's module is perfectly legal for the service that
	// declares the group and is not the others' to satisfy. The resolution
	// already decided it this way; the gate has to agree, or it refuses a plan
	// the resolution would have carried out. Every group in this call is one its
	// consumers did not declare — that is what consumersWithRootGroupsOnly
	// restates — so filtering by group name here is exactly per consumer.
	// (Layer-2 round-six finding 3.)
	rootOnly := make(map[string]bool, len(rootGroups))
	for _, group := range rootGroups {
		rootOnly[group] = true
	}
	rootInfos := credentialsOf(provided.Infos, rootOnly).received(provided.Infos)
	rootProblems := configurations.CheckEndpointReferences(rootInfos, rootConsumers, profile, lookup)
	if merged := mergeUnresolvedReferences(err, rootProblems); merged != nil {
		return merged
	}
	// And the same ambiguity rule the resolution applies, so the gate does not
	// pass a reference the resolution will refuse by name. The alternative is a
	// run that starts, builds and then stops at the first service to read the
	// value — a worse place to learn it.
	return checkPlanReferencesPerConsumer(ctx, consumers, provided.Infos, rootGroups, profile, lookup)
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
//
// It is the ONE implementation of that question. The resolution asks it through
// World.workspaceProducers, which memoizes this for the whole world; a second
// body would be how the plan gate and the resolution come to disagree about
// whether a producer exists — and the lookup is also what decides whether an
// endpoint may cross a module boundary (World.exportableTo), so a disagreement
// is a security one.
func workspaceProducerLookup(ctx context.Context, workspace *resources.Workspace) (configurations.ProducerLookup, error) {
	lookup, _, err := workspaceProducers(ctx, workspace)
	return lookup, err
}

// workspaceProducers reads the workspace's services ONCE and answers both
// questions that depend on them: "is this <module>/<service> a service of this
// workspace", which the plan gate asks, and "what does it declare", which
// core's endpoint selection asks of every resolution.
//
// One read, because they are the same fact. A gate judging a reference against
// one manifest while the resolution selects against another is how the two come
// to disagree about an endpoint, and that disagreement is what this whole area
// has been about.
func workspaceProducers(ctx context.Context, workspace *resources.Workspace) (configurations.ProducerLookup, resources.DeclaredEndpoints, error) {
	if workspace == nil {
		return nil, nil, nil
	}
	services, err := workspace.LoadServices(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read this workspace's services, so the ${endpoint:…} references a service receives cannot be checked against the producers they name: %w", err)
	}
	byUnique := make(map[string]*resources.Service, len(services))
	for _, service := range services {
		identity, idErr := service.Identity()
		if idErr != nil {
			continue
		}
		byUnique[identity.Unique()] = service
	}
	lookup := func(unique string) (*resources.Service, bool) {
		service, ok := byUnique[unique]
		return service, ok
	}
	return lookup, resources.DeclaredEndpointsOf(services), nil
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
//
// The producer is derived HERE, from the value, rather than read off core's
// finding. Core stopped carrying it deliberately: a reference is text from a
// value, a value may be a secret, and a producer taken from a reference's
// tokens is value-derived even when every token is well formed. That rule is
// right and this honours it — the correlation happens in the one place that
// already holds the value, and nothing of it is printed. What reaches the
// reader is the group name and the producer this caller itself named on the
// command line.
func withExcludedProducerReasons(err error, excludedProducers map[string]bool, infos []*basev0.ConfigurationInformation) error {
	if err == nil || len(excludedProducers) == 0 {
		return err
	}
	var unresolved *configurations.UnresolvedReferencesError
	if !errors.As(err, &unresolved) {
		return err
	}
	for i := range unresolved.References {
		reference := &unresolved.References[i]
		producer := ReferenceProducerAt(infos, reference.Group, reference.Key, reference.Position)
		if producer == "" || !excludedProducers[producer] {
			continue
		}
		reference.Reason = fmt.Sprintf(
			"the producer is excluded from this run (--exclude-dependency or a run profile); exclude the %q workspace configuration too, or stop excluding %s",
			reference.Group, producer)
	}
	return err
}

// ReferenceProducerAt is the <module>/<service> named by one value's Nth
// endpoint reference, 1-based, in the order core counts them.
//
// infos MUST be the set the check that produced the position ran over. A
// position is an index into one value's reference list, so a different set —
// one whose value for the same group and key differs because a credential was
// withheld — indexes a different list. A position past the end yields nothing
// rather than a guess, which makes the mismatched-length case safe; the
// same-length case is prevented by the caller binding both to one slice.
//
// It is used in exactly ONE place: naming the producer the OPERATOR excluded on
// their own command line. That is the single permitted echo, and it is permitted
// because what reaches the reader is the operator's own input, matched against
// the derived value rather than printed from it. Nothing else derives a
// producer to display — see cmd/doctor_workspace.go's referenceRemediation for
// the reasoning.
//
// It reads the references the way core does — resources.ConfigurationValueEndpointReferences,
// which covers a templated value's literals and not only its Value — so the
// position core reported indexes the same list. An unparseable or out-of-range
// position yields nothing rather than a guess.
func ReferenceProducerAt(infos []*basev0.ConfigurationInformation, group, key string, position int) string {
	if position < 1 {
		return ""
	}
	for _, info := range infos {
		if info.GetName() != group {
			continue
		}
		for _, value := range info.GetConfigurationValues() {
			if value.GetKey() != key {
				continue
			}
			references := resources.ConfigurationValueEndpointReferences(value)
			if position > len(references) {
				return ""
			}
			parsed, parseErr := resources.ParseEndpoint(references[position-1])
			if parseErr != nil || parsed.Module == "" || parsed.Service == "" {
				return ""
			}
			return parsed.Module + "/" + parsed.Service
		}
	}
	return ""
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
	// the root groups among them — the snapshot NewFlow took, which is also what
	// ordered the dependency graph. One selection, so the graph, this check and
	// the resolution cannot disagree about which producer a reference names.
	//
	// It is read at NewFlow rather than here because the loader the flow itself
	// registers has not Loaded yet at this point in the lifecycle (this gate
	// runs inside InitManagers; Load follows), so neither
	// world.compositionRootGroups nor the manager can name a root group yet.
	//
	// A snapshot that could not be taken refuses the operation. The fallback
	// that used to stand here (a plain disk read plus
	// world.compositionRootWorkspaceConfigurationGroups) was worse than no
	// check: the first cannot see an invocation override, and the second is
	// empty at this point, so a load failure anywhere in the configurations
	// turned the gate into a declared-groups-only check without saying so.
	if flow.providedWorkspaceConfigurationsErr != nil {
		return flow.providedWorkspaceConfigurationsErr
	}
	return CheckConfigurationReferences(ctx, flow.workspace, flow.world.Env, flow.providedWorkspaceConfigurations,
		flow.world.Dependencies, consumers, flow.runProfile, excludedProducers, flow.providedWorkspaceConfigurationRootGroups)
}

// checkPlanReferencesPerConsumer runs core's reference check over every
// consumer of the plan, against the groups that consumer actually receives.
//
// Per consumer, because both halves of the question are: which groups it gets,
// and which of a producer's endpoints its module may reach. The credentials of a
// group it did not declare are removed first, exactly as the resolution removes
// them — a value a service does not receive imposes no obligation on it.
//
// It used to run a CLI-side ambiguity rule here, because core's check judged the
// first matching endpoint while its resolution bound the first matching mapping,
// so neither answered "which endpoint does this reference name". core#702 made
// that one question with one answer, and this calls it: the gate then refuses
// exactly what the resolution would refuse, by construction rather than by two
// implementations being kept in step.
func checkPlanReferencesPerConsumer(
	_ context.Context, consumers []*resources.Service, infos []*basev0.ConfigurationInformation,
	rootGroups []string, profile resources.RunProfile, producers configurations.ProducerLookup,
) error {
	if producers == nil {
		return nil
	}
	excluded := make(map[string]bool, len(profile.ExcludeWorkspaceConfigurations))
	for _, group := range profile.ExcludeWorkspaceConfigurations {
		excluded[group] = true
	}
	for _, consumer := range consumers {
		if consumer == nil {
			continue
		}
		declared := make([]string, 0, len(consumer.WorkspaceConfigurationDependencies))
		own := make(map[string]bool, len(consumer.WorkspaceConfigurationDependencies))
		for _, group := range consumer.WorkspaceConfigurationDependencies {
			if excluded[group] {
				continue
			}
			declared = append(declared, group)
			own[group] = true
		}
		effective := slices.Clone(declared)
		rootOnly := map[string]bool{}
		for _, group := range rootGroups {
			if own[group] || excluded[group] {
				continue
			}
			rootOnly[group] = true
			effective = append(effective, group)
		}
		if len(effective) == 0 {
			continue
		}
		received := credentialsOf(infos, rootOnly).received(infos)
		// The consumer as the resolution sees it: its effective group set,
		// which is what core's check iterates. The workspace's own object is
		// never mutated — it is shared by every consumer of this plan.
		asResolved := *consumer
		asResolved.WorkspaceConfigurationDependencies = effective
		// A zero profile: `effective` already has the exclusions removed, and
		// passing them twice would hide a group from a check it must pass.
		if err := configurations.CheckEndpointReferences(received, []*resources.Service{&asResolved}, resources.RunProfile{}, producers); err != nil {
			return err
		}
	}
	return nil
}
