package gitops

import (
	"context"
	"fmt"
	"strconv"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
)

// resolveDeliveryTarget resolves the host's delivery API from the composition
// identity the environment's host block names — <module>/<service>/<endpoint>
// — to the in-cluster address the delivery Jobs POST to. Nothing about the
// address is declared: the namespace is the one the composition gives the
// host's module, the port is the one the host's service declares for the
// endpoint, and the scheme follows the endpoint's secured flag. A host block
// naming an endpoint the composition does not have, or one that declares no
// port, is refused here, before anything is rendered against it.
func resolveDeliveryTarget(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) (*DeliveryTarget, error) {
	if env == nil || env.Host == nil {
		return nil, nil
	}
	module, serviceName, endpointName := env.Host.DeliveryEndpoint()
	service, err := workspace.LoadService(ctx, &resources.ServiceWithModule{Name: serviceName, Module: module})
	if err != nil {
		return nil, fmt.Errorf("environment %s names %s as its delivery API, which the composition does not have: %w", env.Name, env.Host.Delivery, err)
	}
	var endpoint *resources.Endpoint
	for _, candidate := range service.Endpoints {
		if candidate != nil && candidate.Name == endpointName {
			endpoint = candidate
			break
		}
	}
	if endpoint == nil {
		return nil, fmt.Errorf("environment %s names %s as its delivery API, but service %s/%s declares no endpoint %q", env.Name, env.Host.Delivery, module, serviceName, endpointName)
	}
	ports, err := declaredEndpointPorts(service)
	if err != nil {
		return nil, err
	}
	port, declared := ports[endpointName]
	if !declared {
		return nil, fmt.Errorf("environment %s names %s as its delivery API, but service %s/%s declares no port for endpoint %q (spec.%s.%s); the Jobs that deliver to it cannot dial a port nobody declared",
			env.Name, env.Host.Delivery, module, serviceName, endpointName, deploymentSpecKey, endpointPortsKey)
	}
	namespace := env.ModuleNamespace(workspace, module)
	if namespace == "" {
		return nil, fmt.Errorf("environment %s declares no namespace, so the delivery API %s has no in-cluster address", env.Name, env.Host.Delivery)
	}
	scheme := "http"
	if endpoint.Secured {
		scheme = httpsScheme
	}
	return &DeliveryTarget{
		URL:      scheme + "://" + serviceName + "." + namespace + ".svc.cluster.local:" + strconv.FormatUint(uint64(port), 10),
		Audience: env.Host.Audience,
	}, nil
}
