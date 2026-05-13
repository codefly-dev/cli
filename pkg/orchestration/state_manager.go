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

func NewStateManager(_ context.Context, configurationManager *providers.Manager, dependencies *architecture.ServiceDependencies) (*StateManager, error) {
	return &StateManager{
		dependencies:         dependencies,
		configurationManager: configurationManager,
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
	var confs []*basev0.Configuration
	// We get the shared information from the direct requirements
	requires, err := s.deps().DirectRequires(ctx, service.Unique())
	if err != nil {
		return nil, w.Wrapf(err, "cannot get direct requires")
	}
	var serviceConfigurations []*basev0.Configuration
	var shared []*basev0.Configuration
	for _, req := range requires {
		_, err = resources.ParseServiceWithOptionalModule(req.Unique)
		if err != nil {
			return nil, w.Wrapf(err, "cannot parse service unique")
		}
		shared, err = s.configurationManager.GetSharedServiceConfiguration(ctx, req.Unique)
		if err != nil {
			return nil, w.Wrapf(err, "cannot get shared information")
		}
		serviceConfigurations = append(serviceConfigurations, shared...)

	}
	confs = append(confs, serviceConfigurations...)
	w.Debug("configurations",
		wool.Field("uniqueToService", resources.MakeManyConfigurationSummary(serviceConfigurations)))
	return confs, nil
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

// GetDependenciesEndpoints returns the endpoints for the dependencies of the given service
func (s *StateManager) GetDependenciesEndpoints(ctx context.Context, service *resources.Service) ([]*basev0.Endpoint, error) {
	if s == nil {
		return nil, nil
	}
	w := wool.Get(ctx).In("StateManager.GetDependenciesEndpoints", wool.ThisField(resources.WithUnique(service)))
	s.mu.RLock()
	var endpoints []*basev0.Endpoint
	for _, req := range service.ServiceDependencies {
		endpoints = append(endpoints, s.endpoints[req.Unique()]...)
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

// GetDependenciesNetworkMappings returns the network mappings for the given service
func (s *StateManager) GetDependenciesNetworkMappings(ctx context.Context, service *resources.Service) ([]*basev0.NetworkMapping, error) {
	if s == nil {
		return nil, nil
	}
	w := wool.Get(ctx).In("StateManager.GetDependenciesNetworkMappings", wool.ThisField(resources.WithUnique(service)))
	s.mu.RLock()
	var mappings []*basev0.NetworkMapping
	for _, req := range service.ServiceDependencies {
		mappings = append(mappings, s.networkMappings[req.Unique()]...)
	}
	s.mu.RUnlock()
	w.Debug("got network mappings", wool.Field("mappings", resources.MakeManyNetworkMappingSummary(mappings)))
	return mappings, nil
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
