package manager

import (
	"context"

	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/providers"
	"github.com/codefly-dev/core/wool"
)

// StateManager holds the data that needs to be shared between services
type StateManager struct {
	provider     *providers.Provider
	dependencies *architecture.ServiceDependencies

	endpoints       map[string][]*basev0.Endpoint
	networkMappings map[string][]*basev0.NetworkMapping
}

func NewStateManager(ctx context.Context, provider *providers.Provider, dependencies *architecture.ServiceDependencies) (*StateManager, error) {
	return &StateManager{
		provider:        provider,
		dependencies:    dependencies,
		endpoints:       make(map[string][]*basev0.Endpoint),
		networkMappings: make(map[string][]*basev0.NetworkMapping),
	}, nil
}

// GetProviderInfos returns the provider information for the given service
func (s *StateManager) GetProviderInfos(ctx context.Context, service *configurations.Service) ([]*basev0.ProviderInformation, error) {
	w := wool.Get(ctx).In("service.GetProviderInfos", wool.ThisField(service))
	infos, err := s.provider.GetProviderInformations(ctx, service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get Provider information")
	}
	// We get the shared information from the direct requirements
	requires, err := s.dependencies.DirectRequires(ctx, service.Unique())
	if err != nil {
		return nil, w.Wrapf(err, "cannot get direct requires")
	}
	var uniques []string
	for _, req := range requires {
		uniques = append(uniques, req.Unique)
	}
	shared, err := s.provider.GetSharedInformation(ctx, uniques...)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get shared information")
	}
	infos = append(infos, shared...)
	w.Debug("got provider infos", wool.Field("got", configurations.MakeProviderInformationSummary(infos)))
	return infos, nil
}

// RecordEndpoints records the endpoints for the given service
func (s *StateManager) RecordEndpoints(ctx context.Context, service *configurations.Service, endpoints []*basev0.Endpoint) error {
	w := wool.Get(ctx).In("service.RecordEndpoints", wool.ThisField(service))
	w.Debug("record endpoints", wool.Field("endpoints", configurations.MakeEndpointSummary(endpoints)))
	s.endpoints[service.Unique()] = endpoints
	return nil
}

// GetDependenciesEndpoints returns the endpoints for the dependencies of the given service
func (s *StateManager) GetDependenciesEndpoints(ctx context.Context, service *configurations.Service) ([]*basev0.Endpoint, error) {
	w := wool.Get(ctx).In("service.GetDependenciesEndpoints", wool.ThisField(service))
	var endpoints []*basev0.Endpoint
	for _, req := range service.ServiceDependencies {
		endpoints = append(endpoints, s.endpoints[req.Unique()]...)
	}
	w.Debug("got dependencies endpoints", wool.Field("endpoints", configurations.MakeEndpointSummary(endpoints)))
	return endpoints, nil
}

func (s *StateManager) RecordSharedProviderInfos(ctx context.Context, service *configurations.Service, infos []*basev0.ProviderInformation) error {
	w := wool.Get(ctx).In("service.RecordSharedProviderInfos", wool.ThisField(service))
	w.Debug("record shared provider infos", wool.Field("infos", configurations.MakeProviderInformationSummary(infos)))
	return s.provider.Share(ctx, infos)
}

// GetNetworkMappings returns the network mappings for the given service
func (s *StateManager) GetNetworkMappings(ctx context.Context, service *configurations.Service) ([]*basev0.NetworkMapping, error) {
	w := wool.Get(ctx).In("service.GetNetworkMappings", wool.ThisField(service))
	var mappings []*basev0.NetworkMapping
	for _, req := range service.ServiceDependencies {
		mappings = append(mappings, s.networkMappings[req.Unique()]...)
	}
	w.Debug("got network mappings", wool.Field("mappings", configurations.MakeNetworkMappingSummary(mappings)))
	return mappings, nil
}

// RecordNetworkMappings records the network mappings for the given service
func (s *StateManager) RecordNetworkMappings(ctx context.Context, service *configurations.Service, mappings []*basev0.NetworkMapping) error {
	w := wool.Get(ctx).In("service.RecordNetworkMappings", wool.ThisField(service))
	w.Debug("record network mappings", wool.Field("mappings", configurations.MakeNetworkMappingSummary(mappings)))
	s.networkMappings[service.Unique()] = mappings
	return nil
}

func (s *StateManager) NetworkMappings(unique string) ([]*basev0.NetworkMapping, bool) {
	mappings, ok := s.networkMappings[unique]
	return mappings, ok
}
