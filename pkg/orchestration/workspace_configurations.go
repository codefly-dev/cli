package orchestration

import (
	"context"
	"fmt"
	"maps"
	"slices"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// The workspace configuration groups a service receives are resolved here, once,
// for every path that delivers them. There is one rule:
//
//	the groups a service receives = the groups it declares
//	    (`workspace-configuration-dependencies`)
//	  ∪ the groups the composition root provides run-wide
//	    (core's GetCompositionRootWorkspaceConfigurations)
//	  − the groups the selected run profile excludes
//
// The rendered deployment is the source of truth for that set, and the local run
// resolves the identical one. The two must not differ in either direction. A
// render that resolved less than a run makes "works locally, dials something
// unconfigured once deployed" a property of the pipeline rather than a mistake
// anyone made: the composition root supplies a value every service can read, the
// run delivers it, and the deployed service started without it — naming a key it
// never saw, or not noticing. A run that resolved less than a render would hide
// the opposite fault just as well, so neither path may carry a resolution of its
// own. Only the address family differs, and legitimately: the same group gives a
// native consumer a loopback address and a deployed one its in-cluster address.
//
// The set is selected *before* anything is resolved, and the whole of it drives
// producer discovery. That order is the correctness condition, not a tidiness
// one: core resolves the root's run-wide groups leniently — a value whose
// ${endpoint:…} this consumer cannot resolve is dropped for it rather than
// failing it (#393), and an information block every value of which was dropped
// goes with them. Discovering producers from the declared groups alone therefore
// deletes a root group's endpoint values, and a root group holding nothing else
// disappears whole, with no error anywhere: exactly the omission above, in a new
// place. See effectiveWorkspaceConfigurationGroups and
// refuseUnresolvedWorkspaceConfigurationReferences.
//
// One asymmetry is deliberate and directional: a run profile's
// `exclude-workspace-configurations` trims the run only — profiles never reach a
// build or a deployment — so a profile can leave the run with fewer groups than
// the render, never the reverse, and cannot produce the fault above.
//
// Both delivery paths are below (Runner.workspaceConfigurations,
// Builder.workspaceConfigurations) so that a change to one is a change made in
// sight of the other. The rule is stated for operators in
// `docs/orchestration.md` ("Workspace configuration groups").

// workspaceConfigurationsFor resolves the workspace configurations one service
// receives. ${endpoint:…} references resolve against that service's dependency
// mappings, in the address family of its access, plus the mappings of every
// producer in the run that a configuration of the service's *effective* group
// set names — the groups it declares and the root's alike (see
// referencedProducerMappings).
func (world *World) workspaceConfigurationsFor(
	ctx context.Context, service *resources.Service,
	dependencyMappings []*basev0.NetworkMapping, access *basev0.NetworkAccess,
) ([]*basev0.Configuration, error) {
	declared := make([]string, 0, len(service.WorkspaceConfigurationDependencies))
	for _, dependency := range service.WorkspaceConfigurationDependencies {
		if !world.excludedWorkspaceConfigurations[dependency] {
			declared = append(declared, dependency)
		}
	}
	// The complete set first: a reference carried by a group the service never
	// declared resolves only if the producer it names was discovered, and only
	// this set knows about it.
	effective := world.effectiveWorkspaceConfigurationGroups(declared)
	referenced, err := world.referencedProducerMappings(ctx, service, effective, dependencyMappings)
	if err != nil {
		return nil, err
	}
	mappings := append(slices.Clone(dependencyMappings), referenced...)
	if err = world.refuseUnresolvedWorkspaceConfigurationReferences(ctx, service, effective, mappings); err != nil {
		return nil, err
	}
	manager := world.ConfigurationManager.ForConsumer(mappings, access).WithRunProducers(world.producerInRun())
	resolved, err := manager.GetWorkspaceDependenciesConfigurations(ctx, declared...)
	if err != nil {
		return nil, err
	}
	// The composition root injects its own workspace configurations into every
	// service, so a composed-module service resolves root-provided values
	// without redeclaring them as dependencies. A service that declares a
	// dependency on one of the root's own configurations (e.g. the root service
	// itself) yields that name in both sets, so union by name to avoid emitting
	// it twice.
	root, err := manager.GetCompositionRootWorkspaceConfigurations(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(resolved))
	for _, conf := range resolved {
		for _, info := range conf.Infos {
			seen[info.Name] = true
		}
	}
	out := resolved
	for _, conf := range root {
		if world.workspaceConfigurationExcluded(conf) || world.workspaceConfigurationSeen(conf, seen) {
			continue
		}
		if err = world.requireKnownRootGroup(conf, effective); err != nil {
			return nil, err
		}
		out = append(out, conf)
	}
	return world.applyWorkspaceConfigurationValues(out, declared), nil
}

// effectiveWorkspaceConfigurationGroups is the group set one service receives,
// by name: the groups it declares unioned with the composition root's own,
// minus the profile exclusions, each name once and in a stable order (declared
// first, then the root's, sorted). It is what a resolution must be planned
// against — producer discovery above, and every check that follows.
//
// The root's names are read from the loaders the manager was built with, through
// core's own capability (CompositionRootWorkspaceConfigurationNames), lazily:
// they are populated by the loader's Load, which runs after the World exists.
// A World built without that source names no root group; requireKnownRootGroup
// is what keeps that from passing silently.
func (world *World) effectiveWorkspaceConfigurationGroups(declared []string) []string {
	effective := slices.Clone(declared)
	seen := make(map[string]bool, len(declared))
	for _, group := range declared {
		seen[group] = true
	}
	// Cloned before sorting: the slice is the loader's own, and services
	// initialize concurrently — sorting it in place would mutate shared state
	// from several resolutions at once.
	root := slices.Clone(world.compositionRootWorkspaceConfigurationGroups())
	slices.Sort(root)
	for _, group := range root {
		if seen[group] || world.excludedWorkspaceConfigurations[group] {
			continue
		}
		seen[group] = true
		effective = append(effective, group)
	}
	return effective
}

// compositionRootWorkspaceConfigurationGroups names the composition root's own
// groups, as the manager's loaders report them.
func (world *World) compositionRootWorkspaceConfigurationGroups() []string {
	if world == nil || world.compositionRootGroups == nil {
		return nil
	}
	return world.compositionRootGroups()
}

// requireKnownRootGroup refuses a root group the resolution delivered that the
// effective set did not name. It can only happen when the world's root-name
// source is incomplete — and that source is what producer discovery planned
// against, so every ${endpoint:…} the group carries was resolved against
// mappings nobody bound for it. Shipping the group anyway is shipping the
// omission: a value silently absent from a deployed workload. Refuse instead,
// naming the group and the cause.
func (world *World) requireKnownRootGroup(conf *basev0.Configuration, effective []string) error {
	for _, info := range conf.Infos {
		if slices.Contains(effective, info.Name) {
			continue
		}
		return fmt.Errorf("the composition root provides the workspace configuration group %q run-wide, but this resolution did not plan for it: its ${endpoint:…} references were resolved against no producer mappings, so a value of it may be silently absent. The world was built without a composition-root group source (configurations.Loader.CompositionRootWorkspaceConfigurationNames)", info.Name)
	}
	return nil
}

// refuseUnresolvedWorkspaceConfigurationReferences refuses a ${endpoint:…}
// reference carried by the effective group set that the mappings bound for this
// consumer cannot resolve, where leaving it to core would be a silent omission
// rather than a legitimate drop. Core's own rule tells those apart by the run
// set (configurations.Manager.WithRunProducers): a reference naming a producer
// this run does not contain — excluded infrastructure, or a run of one service
// rather than the workspace — is dropped for the consumer, and no composition
// change would make it resolvable. Those are left alone here.
//
// Two cases are not that, and are refused:
//
//   - The mappings carry other endpoints of the producer but not the one named.
//     The producer's addresses were derived and this endpoint is not among them,
//     so the reference names an endpoint that does not exist. No ordering and no
//     run shape changes that; it is a misreference, in either path.
//   - A render (world.deploys()) derived nothing for a producer the run
//     contains. A deployed address is a pure function of the producer's identity
//     and namespace, so there is no "not yet": nothing to derive means the
//     render would omit the value from a manifest, which is invisible until a
//     client dials it.
//
// A local run that derived nothing for an in-run producer is the one case left
// to core's drop: a producer that has not initialized has recorded no endpoint,
// and the graph only orders a consumer after the producers of the groups it
// *declares* (core's architecture.addConfigurationReferenceEdges reads
// WorkspaceConfigurationDependencies). A root group's reference therefore orders
// nothing, so refusing here would fail a run whose service simply starts first.
// The run can still deliver less than the render that way — the same direction a
// profile exclusion may, and the direction that cannot produce "works locally,
// unconfigured once deployed". It is stated in `docs/orchestration.md`.
func (world *World) refuseUnresolvedWorkspaceConfigurationReferences(
	ctx context.Context, service *resources.Service, groups []string, mappings []*basev0.NetworkMapping,
) error {
	if world == nil || world.ConfigurationManager == nil || len(groups) == 0 {
		return nil
	}
	inRun := world.producerInRun()
	if inRun == nil {
		// Nothing is provably part of the run, so no unresolved reference is
		// provably a fault — the same basis core requires before it may drop.
		return nil
	}
	for _, reference := range world.ConfigurationManager.WorkspaceEndpointReferences(groups...) {
		info, err := resources.ParseEndpoint(reference)
		if err != nil || mappingsCarry(mappings, info) {
			continue
		}
		producer := info.Module + "/" + info.Service
		if !inRun(producer) {
			continue
		}
		consumer := consumerLabel(service)
		if mappingsCarryProducer(mappings, info) {
			return fmt.Errorf("the workspace configuration reference ${endpoint:%s} that %s receives names an endpoint %s does not serve: its other endpoints resolved, so the reference, not the run, is wrong",
				reference, consumer, producer)
		}
		if world.deploys() {
			return fmt.Errorf("cannot resolve the workspace configuration reference ${endpoint:%s} for %s: %s is part of this deployment but no address could be derived for it, so the value would be omitted from the rendered manifest without failing the render",
				reference, consumer, producer)
		}
		wool.Get(ctx).In("World.refuseUnresolvedWorkspaceConfigurationReferences").Debug(
			"a workspace configuration reference does not resolve yet for this consumer: its producer is part of the run and has recorded no endpoint",
			wool.Field("consumer", consumer), wool.Field("producer", producer), wool.Field("reference", reference))
	}
	return nil
}

// consumerLabel names the service a diagnostic is about. resources.WithUnique
// panics on a service whose module was never set, which a diagnostic must not
// do: a partially built service is exactly the state a resolution fault is
// reported from.
func consumerLabel(service *resources.Service) string {
	identity, err := service.Identity()
	if err != nil {
		return service.Name
	}
	return identity.Unique()
}

// mappingsCarryProducer reports whether mappings hold any endpoint of the
// producer a reference names, whatever the endpoint. It is what separates "the
// producer's addresses were never derived" from "they were, and this endpoint is
// not one of them".
func mappingsCarryProducer(mappings []*basev0.NetworkMapping, info *resources.EndpointInformation) bool {
	for _, mapping := range mappings {
		endpoint := mapping.GetEndpoint()
		if endpoint == nil {
			continue
		}
		if endpoint.GetModule() == info.Module && endpoint.GetService() == info.Service {
			return true
		}
	}
	return false
}

// applyWorkspaceConfigurationValues layers the run's derived values onto the
// resolved configurations, for the groups this service actually declares. A
// derived value therefore arrives on the same CODEFLY__WORKSPACE_CONFIGURATION
// carrier a declared one does, which is the contract a service reads — not a
// raw process variable it would only see through an incidental os.Getenv
// fallback.
//
// A derived value replaces a declared one for the same key: these are values
// the run mints for this run (a per-run credential digest), so a stale
// declaration must not win and leave the run's two halves unable to match.
func (world *World) applyWorkspaceConfigurationValues(
	resolved []*basev0.Configuration, dependencies []string,
) []*basev0.Configuration {
	if len(world.workspaceConfigurationValues) == 0 {
		return resolved
	}
	for _, group := range dependencies {
		values := world.workspaceConfigurationValues[group]
		if len(values) == 0 {
			continue
		}
		resolved = upsertWorkspaceConfigurationValues(resolved, group, values)
	}
	return resolved
}

// upsertWorkspaceConfigurationValues sets values on the named group, adding the
// group (and the workspace-origin configuration carrying it) when the
// composition declared none.
func upsertWorkspaceConfigurationValues(
	resolved []*basev0.Configuration, group string, values map[string]string,
) []*basev0.Configuration {
	for _, conf := range resolved {
		if conf.Origin != resources.ConfigurationWorkspace {
			continue
		}
		for _, info := range conf.Infos {
			if info.Name != group {
				continue
			}
			setConfigurationValues(info, values)
			return resolved
		}
	}
	info := &basev0.ConfigurationInformation{Name: group}
	setConfigurationValues(info, values)
	return append(resolved, &basev0.Configuration{
		Origin: resources.ConfigurationWorkspace,
		Infos:  []*basev0.ConfigurationInformation{info},
	})
}

func setConfigurationValues(info *basev0.ConfigurationInformation, values map[string]string) {
	for _, key := range slices.Sorted(maps.Keys(values)) {
		replaced := false
		for _, existing := range info.ConfigurationValues {
			if existing.Key == key {
				existing.Value = values[key]
				replaced = true
				break
			}
		}
		if !replaced {
			info.ConfigurationValues = append(info.ConfigurationValues,
				&basev0.ConfigurationValue{Key: key, Value: values[key]})
		}
	}
}

// workspaceConfigurationSeen reports whether every Info name in a resolved
// workspace configuration is already present in seen (all Infos of a workspace
// configuration share one name), i.e. the configuration was already emitted via
// the declared-dependency set.
func (world *World) workspaceConfigurationSeen(conf *basev0.Configuration, seen map[string]bool) bool {
	for _, info := range conf.Infos {
		if seen[info.Name] {
			return true
		}
	}
	return false
}

// workspaceConfigurationExcluded reports whether a resolved workspace
// configuration is profile-excluded. Workspace configurations carry their name
// on each Info (the Configuration.Origin is always "workspace"), and every Info
// in a given configuration shares that name.
func (world *World) workspaceConfigurationExcluded(conf *basev0.Configuration) bool {
	for _, info := range conf.Infos {
		if world.excludedWorkspaceConfigurations[info.Name] {
			return true
		}
	}
	return false
}

// workspaceConfigurations resolves the groups this running service receives, at
// the addresses matching its runtime context. The run path's only entry point
// into the resolution above.
func (runner *Runner) workspaceConfigurations(
	ctx context.Context, dependencyMappings []*basev0.NetworkMapping, runtimeContext *basev0.RuntimeContext,
) ([]*basev0.Configuration, error) {
	return runner.world.workspaceConfigurationsFor(ctx, runner.instance.Service,
		dependencyMappings, resources.NetworkAccessFromRuntimeContext(runtimeContext))
}

// workspaceConfigurations resolves the groups this deployed service receives, at
// the in-cluster addresses its dependencies and the producers its groups
// reference serve. The render path's only entry point into the resolution above:
// the set is the same one the run resolves, and only the address family differs.
func (b *Builder) workspaceConfigurations(
	ctx context.Context, dependencyMappings []*basev0.NetworkMapping,
) ([]*basev0.Configuration, error) {
	return b.world.workspaceConfigurationsFor(ctx, b.instance.Service,
		dependencyMappings, resources.NewContainerNetworkAccess())
}
