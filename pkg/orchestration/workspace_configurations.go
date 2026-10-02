package orchestration

import (
	"context"
	"maps"
	"slices"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
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
// producer in the run that a configuration the service declares names (see
// referencedProducerMappings).
func (world *World) workspaceConfigurationsFor(
	ctx context.Context, service *resources.Service,
	dependencyMappings []*basev0.NetworkMapping, access *basev0.NetworkAccess,
) ([]*basev0.Configuration, error) {
	dependencies := make([]string, 0, len(service.WorkspaceConfigurationDependencies))
	for _, dependency := range service.WorkspaceConfigurationDependencies {
		if !world.excludedWorkspaceConfigurations[dependency] {
			dependencies = append(dependencies, dependency)
		}
	}
	referenced, err := world.referencedProducerMappings(ctx, service, dependencies, dependencyMappings)
	if err != nil {
		return nil, err
	}
	mappings := append(slices.Clone(dependencyMappings), referenced...)
	manager := world.ConfigurationManager.ForConsumer(mappings, access).WithRunProducers(world.producerInRun())
	declared, err := manager.GetWorkspaceDependenciesConfigurations(ctx, dependencies...)
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
	seen := make(map[string]bool, len(declared))
	for _, conf := range declared {
		for _, info := range conf.Infos {
			seen[info.Name] = true
		}
	}
	out := declared
	for _, conf := range root {
		if world.workspaceConfigurationExcluded(conf) || world.workspaceConfigurationSeen(conf, seen) {
			continue
		}
		out = append(out, conf)
	}
	return world.applyWorkspaceConfigurationValues(out, dependencies), nil
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
