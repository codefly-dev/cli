package manager

import (
	"context"

	"github.com/codefly-dev/core/architecture"
	providers "github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// StateManager holds the data that needs to be shared between services
type StateManager struct {
	configurationManager *providers.Manager
	dependencies         *architecture.ServiceDependencies

	endpoints       map[string][]*basev0.Endpoint
	networkMappings map[string][]*basev0.NetworkMapping
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
func (s *StateManager) GetDependentConfigurationsFor(ctx context.Context, service *resources.Service) ([]*basev0.Configuration, error) {
	if s == nil {
		return nil, nil
	}
	w := wool.Get(ctx).In("StateManager.GetConfigurations", wool.ThisField(service))
	var confs []*basev0.Configuration
	// workspace configurations
	var workspaceConfigurations []*basev0.Configuration
	for _, dep := range service.ConfigurationDependencies {
		conf, err := s.configurationManager.GetConfiguration(ctx, dep)
		if err != nil {
			return nil, w.Wrapf(err, "cannot get workspace configuration")
		}
		workspaceConfigurations = append(workspaceConfigurations, conf)
	}
	confs = append(confs, workspaceConfigurations...)

	// We get the shared information from the direct requirements
	requires, err := s.dependencies.DirectRequires(ctx, service.Unique())
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
		wool.Field("configurations", resources.MakeManyConfigurationSummary(workspaceConfigurations)),
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
	svc, err := s.dependencies.ServiceFromUnique(unique)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get service from unique")
	}
	return s.GetDependentConfigurationsFor(ctx, svc)
}

// RecordEndpoints records the endpoints for the given service
func (s *StateManager) RecordEndpoints(ctx context.Context, service *resources.Service, endpoints []*basev0.Endpoint) error {
	if s == nil {
		return nil
	}
	w := wool.Get(ctx).In("StateManager.RecordEndpoints", wool.ThisField(service))
	w.Debug("record endpoints", wool.Field("endpoints", resources.MakeManyEndpointSummary(endpoints)))
	s.endpoints[service.Unique()] = endpoints
	return nil
}

// GetDependenciesEndpoints returns the endpoints for the dependencies of the given service
func (s *StateManager) GetDependenciesEndpoints(ctx context.Context, service *resources.Service) ([]*basev0.Endpoint, error) {
	if s == nil {
		return nil, nil
	}
	w := wool.Get(ctx).In("StateManager.GetDependenciesEndpoints", wool.ThisField(service))
	var endpoints []*basev0.Endpoint
	for _, req := range service.ServiceDependencies {
		endpoints = append(endpoints, s.endpoints[req.Unique()]...)
	}
	w.Debug("got dependencies endpoints", wool.Field("endpoints", resources.MakeManyEndpointSummary(endpoints)))
	return endpoints, nil
}

// GetNetworkMappings returns the network mappings for the given service
func (s *StateManager) GetNetworkMappings(_ context.Context, service *resources.Service) ([]*basev0.NetworkMapping, error) {
	if s == nil {
		return nil, nil
	}
	return s.networkMappings[service.Unique()], nil
}

func (s *StateManager) GetNetworkMappingsFromUnique(unique string) ([]*basev0.NetworkMapping, bool) {
	mappings, ok := s.networkMappings[unique]
	return mappings, ok
}

// GetDependenciesNetworkMappings returns the network mappings for the given service
func (s *StateManager) GetDependenciesNetworkMappings(ctx context.Context, service *resources.Service) ([]*basev0.NetworkMapping, error) {
	if s == nil {
		return nil, nil
	}
	w := wool.Get(ctx).In("StateManager.GetDependenciesNetworkMappings", wool.ThisField(service))
	var mappings []*basev0.NetworkMapping
	for _, req := range service.ServiceDependencies {
		mappings = append(mappings, s.networkMappings[req.Unique()]...)
	}
	w.Debug("got network mappings", wool.Field("mappings", resources.MakeManyNetworkMappingSummary(mappings)))
	return mappings, nil
}

// RecordNetworkMappings records the network mappings for the given service
func (s *StateManager) RecordNetworkMappings(ctx context.Context, service *resources.Service, mappings []*basev0.NetworkMapping) error {
	if s == nil {
		return nil
	}
	w := wool.Get(ctx).In("StateManager.RecordNetworkMappings", wool.ThisField(service))
	w.Debug("record network mappings", wool.Field("mappings", resources.MakeManyNetworkMappingSummary(mappings)))
	s.networkMappings[service.Unique()] = mappings
	return nil
}
