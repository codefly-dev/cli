package orchestration

import (
	"context"
	"sync"

	"github.com/codefly-dev/core/architecture"
	providers "github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// StateManager holds the data that needs to be shared between services
type StateManager struct {
	mu sync.RWMutex

	configurationManager *providers.Manager

	// dependencies is normally set-once during Flow.InitManagers
	// BEFORE any runner is started, but SetDependencies can be
	// invoked when filters rebuild the graph. Both writes and
	// reads of this pointer go through mu so the race detector
	// stays clean; readers snapshot via deps() and release the
	// lock before calling methods on the snapshot.
	dependencies *architecture.ServiceDependencies

	// workspace is the composition every hand-out of dependency addresses is
	// judged with (resources.Provenance): the CLI is the provider of every
	// address a service, a job or an SDK process receives, and it holds the
	// composition, so the verdict runs here with it.
	workspace *resources.Workspace

	endpoints       map[string][]*basev0.Endpoint
	networkMappings map[string][]*basev0.NetworkMapping
}

// SetDependencies rebinds the dependency graph. Used by Flow when
// remote/exclude filters change the graph after construction.
func (s *StateManager) SetDependencies(deps *architecture.ServiceDependencies) {
	s.mu.Lock()
	s.dependencies = deps
	s.mu.Unlock()
}

// deps returns the current dependencies pointer under RLock and
// releases the lock before returning. Callers can safely call
// methods on the returned pointer; ServiceDependencies itself is
// internally immutable post-construction so there's no further
// read-side lock to take.
func (s *StateManager) deps() *architecture.ServiceDependencies {
	s.mu.RLock()
	d := s.dependencies
	s.mu.RUnlock()
	return d
}

// NewStateManager builds the shared state of a flow over the workspace that
// composes it: the provenance every dependency hand-out is judged with.
func NewStateManager(_ context.Context, configurationManager *providers.Manager, dependencies *architecture.ServiceDependencies, workspace *resources.Workspace) (*StateManager, error) {
	return &StateManager{
		dependencies:         dependencies,
		configurationManager: configurationManager,
		workspace:            workspace,
		endpoints:            make(map[string][]*basev0.Endpoint),
		networkMappings:      make(map[string][]*basev0.NetworkMapping),
	}, nil
}

// GetDependentConfigurationsFor returns the configurations for the given service
// It includes configuration from its dependencies
func (s *StateManager) GetDependentConfigurationsFor(ctx context.Context, service *resources.ServiceIdentity) ([]*basev0.Configuration, error) {
	if s == nil {
		return nil, nil
	}
	w := wool.Get(ctx).In("StateManager.GetConfigurations", wool.ThisField(service))
	// We get the shared information from the direct requirements
	requires, err := s.deps().DirectRequires(ctx, service.Unique())
	if err != nil {
		return nil, w.Wrapf(err, "cannot get direct requires")
	}
	// A configuration is a runtime value in exactly the sense a network mapping
	// is: it is the producer's live connection, exposed by its agent while it
	// runs. A dependency that does not constrain the run stage therefore has
	// none to consume, and injecting it anyway is what core's dependency
	// resolver refuses for the matching endpoint — "credentials for a service it
	// only builds against", handed over only when some other service happens to
	// be running it. Filtering here rather than at either writer keeps the
	// agent's Init request and the --output-env file carrying the same set: a
	// narrower file would just move the disagreement, not remove it.
	consumesAtRun, err := s.runStageDependencies(service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot resolve run-stage dependencies")
	}
	var serviceConfigurations []*basev0.Configuration
	var shared []*basev0.Configuration
	for _, req := range requires {
		_, err = resources.ParseServiceWithOptionalModule(req.Unique)
		if err != nil {
			return nil, w.Wrapf(err, "cannot parse service unique")
		}
		if !consumesAtRun[req.Unique] {
			continue
		}
		shared, err = s.configurationManager.GetSharedServiceConfiguration(ctx, req.Unique)
		if err != nil {
			return nil, w.Wrapf(err, "cannot get shared information")
		}
		serviceConfigurations = append(serviceConfigurations, shared...)

	}
	confs := make([]*basev0.Configuration, 0, len(serviceConfigurations))
	confs = append(confs, serviceConfigurations...)
	w.Debug("configurations",
		wool.Field("uniqueToService", resources.MakeManyConfigurationSummary(serviceConfigurations)))
	return confs, nil
}

// runStageDependencies is the set of a consumer's declared dependencies that
// constrain the run stage, keyed by unique. A legacy (untyped) dependency
// participates in every stage, so an existing workspace keeps every
// configuration it had; only an explicitly build-only, schema-only or external
// edge is excluded.
func (s *StateManager) runStageDependencies(service *resources.ServiceIdentity) (map[string]bool, error) {
	consumer, err := s.deps().ServiceFromUnique(service.Unique())
	if err != nil {
		return nil, err
	}
	consumesAtRun := make(map[string]bool, len(consumer.ServiceDependencies))
	for _, dependency := range consumer.ServiceDependencies {
		if dependency.Participates(resources.StageRun) {
			consumesAtRun[dependency.Unique()] = true
		}
	}
	return consumesAtRun, nil
}

// GetDependentConfigurationsForUnique returns the configurations for the given service
// It includes configuration from its dependencies
func (s *StateManager) GetDependentConfigurationsForUnique(ctx context.Context, unique string) ([]*basev0.Configuration, error) {
	if s == nil {
		return nil, nil
	}
	w := wool.Get(ctx).In("StateManager.GetDependentConfigurationsForUnique", wool.Field("this", unique))
	svc, err := s.deps().ServiceFromUnique(unique)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get service from unique")
	}
	id, err := svc.Identity()
	if err != nil {
		return nil, w.Wrapf(err, "cannot get identity")
	}
	return s.GetDependentConfigurationsFor(ctx, id)
}

// RecordEndpoints records the endpoints for the given service
func (s *StateManager) RecordEndpoints(ctx context.Context, service *resources.ServiceIdentity, endpoints []*basev0.Endpoint) error {
	if s == nil {
		return nil
	}
	w := wool.Get(ctx).In("StateManager.RecordEndpoints", wool.ThisField(service))
	w.Debug("record endpoints", wool.Field("endpoints", resources.MakeManyEndpointSummary(endpoints)))
	s.mu.Lock()
	s.endpoints[service.Unique()] = endpoints
	s.mu.Unlock()
	return nil
}

// RecordedEndpoints returns the endpoints a service of the run recorded at Load.
func (s *StateManager) RecordedEndpoints(unique string) []*basev0.Endpoint {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.endpoints[unique]
}

// GetDependenciesEndpoints returns the endpoints for the dependencies of the given service
func (s *StateManager) GetDependenciesEndpoints(ctx context.Context, service *resources.Service) ([]*basev0.Endpoint, error) {
	if s == nil {
		return nil, nil
	}
	w := wool.Get(ctx).In("StateManager.GetDependenciesEndpoints", wool.ThisField(resources.WithUnique(service)))
	s.mu.RLock()
	var endpoints []*basev0.Endpoint
	for _, req := range service.ServiceDependencies {
		for _, endpoint := range s.endpoints[req.Unique()] {
			if endpoint == nil || dependencyConsumesEndpoint(req, endpoint) {
				endpoints = append(endpoints, endpoint)
			}
		}
	}
	s.mu.RUnlock()
	w.Debug("got dependencies endpoints", wool.Field("endpoints", resources.MakeManyEndpointSummary(endpoints)))
	return endpoints, nil
}

// GetNetworkMappings returns the network mappings for the given service
func (s *StateManager) GetNetworkMappings(_ context.Context, service *resources.ServiceIdentity) ([]*basev0.NetworkMapping, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.networkMappings[service.Unique()], nil
}

func (s *StateManager) GetNetworkMappingsFromUnique(unique string) ([]*basev0.NetworkMapping, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	mappings, ok := s.networkMappings[unique]
	return mappings, ok
}

// GetDependenciesNetworkMappings hands a consumer the addresses of what its
// dependencies consume, JUDGED. The CLI is the provider of every address a
// service, a job or an SDK process receives, and it holds the composition, so
// the one verdict runs here with the composition's provenance
// (resources.ResolveDependencyNetworkMappings): the edge by the provenance of
// its two ends — a solution reaches modules only through the host — then each
// endpoint by the producer's export, and only a run-stage dependency has an
// address to consume at all. Core's own wrappers judge the agent requests
// again with Instance.Workspace (services.RuntimeInstance.Init/Start,
// services.BuilderInstance.Deploy); what the SDK reads back through
// GetDependenciesNetworkMappings (pkg/web/go-grpc) has no second verdict, so
// this one is the one that holds for it. It used to narrow by the dependency's
// endpoint list alone, never by visibility, which core#717 named as the CLI's
// unjudged provider.
func (s *StateManager) GetDependenciesNetworkMappings(ctx context.Context, service *resources.Service) ([]*basev0.NetworkMapping, error) {
	if s == nil {
		return nil, nil
	}
	w := wool.Get(ctx).In("StateManager.GetDependenciesNetworkMappings", wool.ThisField(resources.WithUnique(service)))
	identity, err := service.Identity()
	if err != nil {
		return nil, w.Wrapf(err, "cannot judge dependency network mappings for a consumer with no identity")
	}
	s.mu.RLock()
	var candidates []*basev0.NetworkMapping
	for _, req := range service.ServiceDependencies {
		for _, mapping := range s.networkMappings[req.Unique()] {
			if mapping == nil {
				continue
			}
			candidates = append(candidates, mapping)
		}
	}
	// A nil *Workspace must reach core as a nil Provenance, so the refusal
	// says "judged with no composition" rather than naming a member it never
	// had.
	var provenance resources.Provenance
	if s.workspace != nil {
		provenance = s.workspace
	}
	s.mu.RUnlock()
	mappings, err := resources.ResolveDependencyNetworkMappings(provenance, identity.Module, service.ServiceDependencies, candidates)
	if err != nil {
		return nil, w.Wrap(err)
	}
	w.Debug("got network mappings", wool.Field("mappings", resources.MakeManyNetworkMappingSummary(mappings)))
	return mappings, nil
}

// dependencyConsumesEndpoint keeps the runtime capability set faithful to the
// service manifest. An empty endpoint list is the current contract for all
// producer endpoints; an explicit list grants only those declared capabilities.
// It is the single selector for "does this consumer consume this endpoint":
// readiness gates on exactly what this hands to the consumer, so the two can
// never disagree about which endpoints matter. Matching is by name and API and
// never by owning module/service — those fields cross an agent's gRPC boundary,
// while the mapping list is already scoped to one producer.
func dependencyConsumesEndpoint(dependency *resources.ServiceDependency, endpoint *basev0.Endpoint) bool {
	return dependency.ConsumesEndpoint(endpoint.GetName(), endpoint.GetApi())
}

// RecordNetworkMappings records the network mappings for the given service
func (s *StateManager) RecordNetworkMappings(ctx context.Context, service *resources.Service, mappings []*basev0.NetworkMapping) error {
	if s == nil {
		return nil
	}
	w := wool.Get(ctx).In("StateManager.RecordNetworkMappings", wool.ThisField(resources.WithUnique(service)))
	w.Debug("record network mappings", wool.Field("mappings", resources.MakeManyNetworkMappingSummary(mappings)))
	id, err := service.Identity()
	if err != nil {
		return w.Wrapf(err, "cannot get service identity")
	}
	s.mu.Lock()
	s.networkMappings[id.Unique()] = mappings
	s.mu.Unlock()
	return nil
}
