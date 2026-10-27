package environments

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// EnvironmentExcludedService withdraws one service of a composed module from an
// environment: it is not rendered, built, published, given a unit in the
// inventory or a workload in the cell file there. The composition decides it;
// the module's own service list is untouched, so every other environment (a
// local run included) still composes the service.
//
// Nothing may still consume what is withdrawn. A service that declares a
// dependency on it is refused by name unless WithdrawnFrom names that consumer,
// which withdraws the consumer's edge with the service — the consumer is handed
// no address for it, and no mesh edge, network policy or managed identity is
// derived from that edge. An environment declaration that names the service
// (ingress, host, cell, egress, service-mesh, service-identity, service-config,
// service-secrets, managed-services) is refused, and so is a workspace
// configuration reference to one of its endpoints, at plan time: such a value
// would resolve to nothing.
//
// A service the module's interface exports, or names its service-entry, is
// withdrawn only with the whole module: an environment that excludes every
// service of a module leaves the module out, its exports with it, while one
// that excludes only some of them is refused, because the module it still
// composes would promise an endpoint that is not there.
type EnvironmentExcludedService struct {
	// Service is the "<module>/<service>" withdrawn; never a bare name, so one
	// entry can only ever withdraw one service.
	Service string `yaml:"service"`
	// WithdrawnFrom names, as "<module>/<service>", each consumer whose declared
	// dependency on Service is withdrawn with it. Every consumer must be named,
	// and every name must be a consumer.
	WithdrawnFrom []string `yaml:"withdrawn-from,omitempty"`
}

// UnmarshalYAML refuses an unknown key: a mistyped `withdrawn-from` would
// otherwise read as no consumer at all.
func (excluded *EnvironmentExcludedService) UnmarshalYAML(node *yaml.Node) error {
	if err := rejectUnknownKeys(node, "exclude-services", "service", "withdrawn-from"); err != nil {
		return err
	}
	type plain EnvironmentExcludedService
	return node.Decode((*plain)(excluded))
}

// Excludes reports whether this environment withdraws module/service.
func (env *Environment) Excludes(module, service string) bool {
	if env == nil {
		return false
	}
	unique := resources.ServiceUnique(module, service)
	for _, excluded := range env.ExcludeServices {
		if excluded.Service == unique {
			return true
		}
	}
	return false
}

// ComposedServices returns the module's service references this environment
// composes: the module's own list less what it excludes. Every loop over a
// module's services for one environment — render, build, deploy — reads it.
func (env *Environment) ComposedServices(module *resources.Module) []*resources.ServiceReference {
	if module == nil {
		return nil
	}
	composed := make([]*resources.ServiceReference, 0, len(module.ServiceReferences))
	for _, reference := range module.ServiceReferences {
		if reference != nil && !env.Excludes(module.Name, reference.Name) {
			composed = append(composed, reference)
		}
	}
	return composed
}

// withdrawsModule reports whether this environment excludes every service of
// the module: the module is then not composed here at all, so neither its
// interface exports nor its service-entry are offered to the composition.
func (env *Environment) withdrawsModule(module *resources.Module) bool {
	if env == nil || module == nil || len(module.ServiceReferences) == 0 {
		return false
	}
	for _, reference := range module.ServiceReferences {
		if reference != nil && !env.Excludes(module.Name, reference.Name) {
			return false
		}
	}
	return true
}

// ExcludedServiceUniques returns the "<module>/<service>" this environment
// withdraws, sorted: the form architecture.ExcludeServices keys on.
func (env *Environment) ExcludedServiceUniques() []string {
	if env == nil || len(env.ExcludeServices) == 0 {
		return nil
	}
	uniques := make([]string, 0, len(env.ExcludeServices))
	for _, excluded := range env.ExcludeServices {
		uniques = append(uniques, excluded.Service)
	}
	sort.Strings(uniques)
	return uniques
}

// validateExcludedServices holds the block to its own shape: qualified names, no
// repeats, no service withdrawn from itself.
func (env *Environment) validateExcludedServices() error {
	seen := map[string]bool{}
	for _, excluded := range env.ExcludeServices {
		if err := validateQualifiedService("exclude-services service", excluded.Service); err != nil {
			return err
		}
		if seen[excluded.Service] {
			return fmt.Errorf("exclude-services names %q twice", excluded.Service)
		}
		seen[excluded.Service] = true
		consumers := map[string]bool{}
		for _, consumer := range excluded.WithdrawnFrom {
			if err := validateQualifiedService(fmt.Sprintf("exclude-services %q withdrawn-from", excluded.Service), consumer); err != nil {
				return err
			}
			if consumer == excluded.Service || consumers[consumer] {
				return fmt.Errorf("exclude-services %q withdrawn-from names %q twice or names the service itself", excluded.Service, consumer)
			}
			consumers[consumer] = true
		}
	}
	return nil
}

func validateQualifiedService(label, value string) error {
	module, service, qualified := strings.Cut(value, "/")
	if !qualified {
		return fmt.Errorf("%s %q must be <module>/<service>", label, value)
	}
	if err := validateResourcePathComponent(label+" module", module); err != nil {
		return err
	}
	return validateResourcePathComponent(label, service)
}

// ValidateEnvironmentExclusions is ValidateExcludedServices for the one
// environment a run, build or deploy selected: a flow is held to its own
// environment's composition, never refused over another's.
func ValidateEnvironmentExclusions(ctx context.Context, workspace *resources.Workspace, env *Environment) error {
	if env == nil || len(env.ExcludeServices) == 0 {
		return nil
	}
	graph, err := loadExclusionGraph(ctx, workspace)
	if err != nil {
		return err
	}
	if err := env.validateExclusionsAgainst(graph); err != nil {
		return fmt.Errorf("environment %q: %w", env.Name, err)
	}
	return nil
}

// ValidateExcludedServices holds every environment's exclude-services to the
// workspace graph: each names a composed service, every service that still
// consumes it is a declared withdrawal, and no other declaration of the
// environment names it. It loads the services only when an environment
// excludes one.
func ValidateExcludedServices(ctx context.Context, workspace *resources.Workspace) error {
	declared, err := FromWorkspace(workspace)
	if err != nil {
		return err
	}
	var graph *exclusionGraph
	for _, env := range declared {
		if env == nil || len(env.ExcludeServices) == 0 {
			continue
		}
		if graph == nil {
			if graph, err = loadExclusionGraph(ctx, workspace); err != nil {
				return err
			}
		}
		if err := env.validateExclusionsAgainst(graph); err != nil {
			return fmt.Errorf("environment %q: %w", env.Name, err)
		}
	}
	return nil
}

// exclusionGraph is what an exclusion is held to: every composed service, every
// module (its interface and service-entry), and what each composed solution
// consumes through its manifest.
type exclusionGraph struct {
	services []*resources.Service
	modules  []*resources.Module
	consumed []solutionrun.Consumption
}

func loadExclusionGraph(ctx context.Context, workspace *resources.Workspace) (*exclusionGraph, error) {
	services, err := workspace.LoadServices(ctx)
	if err != nil {
		return nil, err
	}
	modules, err := workspace.LoadModules(ctx)
	if err != nil {
		return nil, err
	}
	consumed, err := solutionrun.ConsumedEndpoints(ctx, workspace)
	if err != nil {
		return nil, err
	}
	return &exclusionGraph{services: services, modules: modules, consumed: consumed}, nil
}

func (env *Environment) validateExclusionsAgainst(graph *exclusionGraph) error {
	services, modules := graph.services, graph.modules
	composed := map[string]bool{}
	byName := map[string][]string{}
	// consumers[target] are the services declaring a dependency on target.
	consumers := map[string][]string{}
	for _, service := range services {
		identity, err := service.Identity()
		if err != nil {
			return err
		}
		unique := identity.Unique()
		composed[unique] = true
		byName[identity.Name] = append(byName[identity.Name], unique)
		for _, dependency := range service.ServiceDependencies {
			if dependency == nil {
				continue
			}
			module := dependency.Module
			if module == "" {
				module = identity.Module
			}
			target := resources.ServiceUnique(module, dependency.Name)
			if !slices.Contains(consumers[target], unique) {
				consumers[target] = append(consumers[target], unique)
			}
		}
	}
	excluded := map[string]bool{}
	for _, entry := range env.ExcludeServices {
		excluded[entry.Service] = true
	}
	for _, entry := range env.ExcludeServices {
		if !composed[entry.Service] {
			return fmt.Errorf("exclude-services names %q, which the workspace does not compose", entry.Service)
		}
		for _, consumer := range consumers[entry.Service] {
			if excluded[consumer] || slices.Contains(entry.WithdrawnFrom, consumer) {
				continue
			}
			return fmt.Errorf("exclude-services withdraws %q, but %s declares a dependency on it: name %s under its withdrawn-from to withdraw that edge with it, or stop excluding %s",
				entry.Service, consumer, consumer, entry.Service)
		}
		for _, consumer := range entry.WithdrawnFrom {
			if !slices.Contains(consumers[entry.Service], consumer) {
				return fmt.Errorf("exclude-services %q withdrawn-from names %s, which declares no dependency on it", entry.Service, consumer)
			}
			if excluded[consumer] {
				return fmt.Errorf("exclude-services %q withdrawn-from names %s, which this environment excludes too", entry.Service, consumer)
			}
		}
	}
	// A module interface exporting one of its endpoints, or naming it the
	// module's entry, promises the composition what this environment does
	// not have; so does a solution consuming one of its endpoints. A module
	// this environment withdraws whole makes no promise here: its exports and
	// its entry leave with it, and whatever still reached them is a consumer
	// the checks above and below refuse by name.
	for _, module := range modules {
		if module == nil || env.withdrawsModule(module) {
			continue
		}
		if module.ServiceEntry != "" && excluded[resources.ServiceUnique(module.Name, module.ServiceEntry)] {
			return fmt.Errorf("module %s names %s/%s its service-entry, which exclude-services withdraws from this environment: exclude every service of module %s to leave the module out, or stop excluding %s/%s", module.Name, module.Name, module.ServiceEntry, module.Name, module.Name, module.ServiceEntry)
		}
		if module.Interface == nil {
			continue
		}
		for _, exported := range module.Interface.Endpoints {
			if exported != nil && excluded[resources.ServiceUnique(module.Name, exported.Service)] {
				return fmt.Errorf("module %s exports %s/%s/%s through its interface, which exclude-services withdraws from this environment: exclude every service of module %s to leave the module out, or stop excluding %s/%s", module.Name, module.Name, exported.Service, exported.Endpoint, module.Name, module.Name, exported.Service)
			}
		}
	}
	for index := range graph.consumed {
		consumption := &graph.consumed[index]
		if excluded[resources.ServiceUnique(consumption.Module, consumption.Service)] {
			return fmt.Errorf("solution %s consumes %s/%s/%s (api.consumes), which exclude-services withdraws from this environment", consumption.Solution, consumption.Module, consumption.Service, consumption.Endpoint)
		}
	}
	for _, named := range env.serviceDeclarations() {
		target := named.name
		if !strings.Contains(target, "/") {
			// A bare name declares the service it resolves to, which is a
			// withdrawn one only when no other module carries the name.
			if candidates := byName[target]; len(candidates) == 1 {
				target = candidates[0]
			}
		}
		if excluded[target] {
			return fmt.Errorf("%s names %s, which exclude-services withdraws from this environment: drop the declaration, or stop excluding %s", named.block, named.name, target)
		}
	}
	return nil
}

type namedService struct{ block, name string }

// serviceDeclarations lists every service the environment's own declarations
// name, by the block naming it. Host and ingress entries name an endpoint or a
// route through "<module>/<service>[/<endpoint>]".
func (env *Environment) serviceDeclarations() []namedService {
	var named []namedService
	for block, names := range env.serviceScopedNames() {
		for _, name := range names {
			named = append(named, namedService{block, name})
		}
	}
	for key := range env.ManagedServices {
		named = append(named, namedService{"managed-services", key})
	}
	for key := range env.Egress {
		named = append(named, namedService{"egress", key})
	}
	for key := range env.Cell {
		named = append(named, namedService{"cell", key})
	}
	for _, route := range env.Ingress {
		named = append(named, namedService{fmt.Sprintf("ingress route %q", route.Name), route.Service})
	}
	if env.ServiceSecrets != nil {
		for _, generator := range env.ServiceSecrets.Generate {
			for _, service := range generator.Services {
				named = append(named, namedService{"service-secrets generate", service})
			}
		}
	}
	if env.Host != nil {
		for block, value := range map[string]string{"host.gateway": env.Host.Gateway, "host.delivery": env.Host.Delivery} {
			if parts := strings.SplitN(value, "/", 3); len(parts) >= 2 {
				named = append(named, namedService{block, parts[0] + "/" + parts[1]})
			}
		}
	}
	sort.Slice(named, func(i, j int) bool {
		if named[i].block != named[j].block {
			return named[i].block < named[j].block
		}
		return named[i].name < named[j].name
	})
	return named
}
