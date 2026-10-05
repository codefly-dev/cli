package orchestration

import (
	"context"
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
// the environment provides it, against the producers the WORKSPACE declares,
// and lists every unresolved reference in one error (core's
// configurations.CheckEndpointReferences). The same references fail again when a
// value is resolved: that is the last line of defence, this is the first.
//
// The workspace, not the run (core's ProducerLookup): a producer a run excludes
// (--exclude-dependency, a run profile) or simply does not start is still a
// real service, and a reference naming it is a fact about the composition that
// holds whatever subset is being run. Whether a producer is in THIS run is the
// resolve-time question — core's strict path drops, for this consumer, a value
// whose reference names a producer outside the run, and says so.
//
// provided may be a read the caller already has; nil reads it here.
// profile is the run profile the caller resolved; the groups it excludes are
// never received by a consumer (a zero profile excludes nothing), and core reads
// them from it rather than from a set assembled here.
func CheckConfigurationReferences(
	ctx context.Context, workspace *resources.Workspace, env *environments.Environment,
	provided *configurations.WorkspaceConfigurations, consumers []*resources.Service,
	profile resources.RunProfile,
) error {
	if workspace == nil || env == nil || len(consumers) == 0 {
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
	lookup, err := workspaceProducerLookup(ctx, workspace)
	if err != nil {
		return err
	}
	return configurations.CheckEndpointReferences(provided.Infos, consumers, profile, lookup)
}

// ReferenceProducerAt is the <module>/<service> named by one value's Nth
// endpoint reference, 1-based, in the order core counts them.
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

// workspaceProducerLookup resolves <module>/<service> over the whole workspace.
// It fails closed: an unreadable workspace means no reference can be checked,
// and a reference delivered unchecked is how a private endpoint's address
// reaches a service that may not see it.
func workspaceProducerLookup(ctx context.Context, workspace *resources.Workspace) (configurations.ProducerLookup, error) {
	if workspace == nil {
		return nil, nil
	}
	services, err := workspace.LoadServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot read this workspace's services, so the ${endpoint:…} references a service receives cannot be checked against the producers they name: %w", err)
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
	return CheckConfigurationReferences(ctx, workspace, env, provided, consumers, resources.RunProfile{})
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
	return CheckConfigurationReferences(ctx, flow.workspace, flow.world.Env, flow.providedWorkspaceConfigurations,
		consumers, flow.runProfile)
}
