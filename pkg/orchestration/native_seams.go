package orchestration

import (
	"context"
	"fmt"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

// ProposedEndpointPort answers where a producer's endpoint will listen on this
// host: the port it recorded once it initialized, or the one its own Init will
// propose, which a run without --temporary-ports derives deterministically.
// A native run standing in for a cluster uses it to tell one service where
// another one serves before the other has started.
func (flow *Flow) ProposedEndpointPort(ctx context.Context, producer, endpoint, api string) (int, error) {
	if flow == nil || flow.world == nil {
		return 0, fmt.Errorf("no flow")
	}
	mappings, err := flow.world.producerNetworkMappings(ctx, producer)
	if err != nil {
		return 0, err
	}
	if len(mappings) == 0 {
		service, err := flow.world.Dependencies.ServiceFromUnique(producer)
		if err != nil {
			return 0, err
		}
		identity, err := service.Identity()
		if err != nil {
			return 0, err
		}
		endpoints, err := service.LoadEndpoints(ctx)
		if err != nil {
			return 0, err
		}
		if mappings, err = flow.world.localProducerMappings(ctx, service, identity, endpoints); err != nil {
			return 0, err
		}
	}
	for _, mapping := range mappings {
		if mapping.GetEndpoint() == nil || mapping.GetEndpoint().GetName() != endpoint {
			continue
		}
		if api != "" && mapping.GetEndpoint().GetApi() != api {
			continue
		}
		for _, instance := range mapping.GetInstances() {
			if instance.GetAccess() != nil && instance.GetAccess().GetKind() == resources.NetworkAccessNative && instance.GetPort() > 0 {
				return int(instance.GetPort()), nil
			}
		}
		for _, instance := range mapping.GetInstances() {
			if instance.GetPort() > 0 {
				return int(instance.GetPort()), nil
			}
		}
	}
	return 0, fmt.Errorf("%s proposes no %s/%s endpoint", producer, endpoint, api)
}

var _ = basev0.NetworkAccess{}

// ResolveWorkspaceGroupFor resolves one workspace configuration group as the
// given service would receive it: through the same manager, scoped to the
// same consumer module. A native run standing in for a cluster uses it to
// alias one service's secret to another, as a cell's render aliases a Secret.
func (world *World) ResolveWorkspaceGroupFor(ctx context.Context, service *resources.Service, group string) (*basev0.ConfigurationInformation, error) {
	if world == nil || world.ConfigurationManager == nil {
		return nil, fmt.Errorf("no configuration manager")
	}
	consumerModule := ""
	if identity, err := service.Identity(); err == nil {
		consumerModule = identity.Module
	}
	declaredEndpoints, err := world.declaredEndpointsLookup(ctx)
	if err != nil {
		return nil, err
	}
	manager := world.ConfigurationManager.ForConsumer(nil, nil).
		ForConsumerModule(consumerModule, declaredEndpoints).
		WithRunProducers(world.producerInRun())
	resolved, err := manager.GetWorkspaceDependenciesConfigurations(ctx, group)
	if err != nil {
		return nil, err
	}
	for _, conf := range resolved {
		for _, info := range conf.Infos {
			if info.Name == group {
				return info, nil
			}
		}
	}
	return nil, fmt.Errorf("workspace configuration %q resolves to nothing for %s", group, resources.WithUnique(service).Unique())
}
