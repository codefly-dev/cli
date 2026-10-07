package gitops

import (
	"context"
	"fmt"
	"strconv"

	"github.com/codefly-dev/cli/pkg/environments"
	corenetwork "github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
)

// resolveDeliveryTarget resolves the host's delivery API from the composition
// identity the environment's host block names — <module>/<service>/<endpoint>
// — to the in-cluster address the delivery Jobs POST to. Nothing about the
// address is declared: the namespace is the one the composition gives the
// host's module, the port is the SERVICE port the CLI allocates for that
// endpoint (core's network.DeployedEndpointPorts, the same allocation the
// agent renders its Service with — not the container port the service
// declares, which is the pod's and may differ), and the scheme follows the
// endpoint's secured flag. A host block naming an endpoint the composition
// does not have is refused here, before anything is rendered against it.
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
	endpoints, err := service.DependencyEndpoints()
	if err != nil {
		return nil, fmt.Errorf("environment %s names %s as its delivery API, but its endpoints cannot be read: %w", env.Name, env.Host.Delivery, err)
	}
	ports, err := corenetwork.DeployedEndpointPorts(ctx, module, serviceName, endpoints)
	if err != nil {
		return nil, fmt.Errorf("environment %s names %s as its delivery API, but its Service port cannot be allocated: %w", env.Name, env.Host.Delivery, err)
	}
	port, allocated := ports[endpointName]
	if !allocated {
		return nil, fmt.Errorf("environment %s names %s as its delivery API, but no Service port is allocated for endpoint %q", env.Name, env.Host.Delivery, endpointName)
	}
	namespace := env.ModuleNamespace(workspace, module)
	if namespace == "" {
		return nil, fmt.Errorf("environment %s declares no namespace, so the delivery API %s has no in-cluster address", env.Name, env.Host.Delivery)
	}
	// Plain HTTP inside the cluster: the mesh carries it over mTLS between the
	// Job's pod and the host's, and the bearer token never leaves that
	// tunnel. An endpoint the host serves over TLS itself is marked secured
	// and dialled with https.
	scheme := "http"
	if endpoint.Secured {
		scheme = httpsScheme
	}
	return &DeliveryTarget{
		URL:      scheme + "://" + serviceName + "." + namespace + ".svc.cluster.local:" + strconv.FormatUint(uint64(port), 10),
		Audience: env.Host.Audience,
	}, nil
}
