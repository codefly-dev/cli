package network

import (
	"context"

	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/wool"
)

// A ServiceManager helps go from a service to applications endpoint instances
type ServiceManager struct {
	endpoints []*basev0.Endpoint

	strategy Strategy
	specs    []*ApplicationEndpoint

	ids  map[string]int
	host string

	reserved *ApplicationEndpointInstances
	external map[string]string
}

func NewServiceManager(endpoints ...*basev0.Endpoint) *ServiceManager {
	return &ServiceManager{
		endpoints: endpoints,
		strategy:  &FixedStrategy{},
		ids:       make(map[string]int),
		external:  make(map[string]string),
	}
}

func (pm *ServiceManager) Bind(ctx context.Context, endpoint *basev0.Endpoint, portBinding string) error {
	w := wool.Get(ctx).In("ServiceManager.Bind")
	if endpoint == nil {
		return w.NewError("cannot bind nil endpoint")
	}
	w = w.With(wool.Field("endpoint", endpoint.Name))

	w.Trace("binding endpoint")
	pm.specs = append(pm.specs,
		&ApplicationEndpoint{
			Name:        endpoint.Name,
			Service:     endpoint.Service,
			Application: endpoint.Application,
			Namespace:   endpoint.Namespace,
			Endpoint:    endpoint,
			PortBinding: portBinding,
		})
	pm.ids[ToUnique(endpoint)]++
	return nil
}

func (pm *ServiceManager) Expose(endpoint *basev0.Endpoint) error {
	w := wool.Get(context.Background()).In("ServiceManager.Expose")
	if endpoint == nil {
		return w.NewError("cannot expose nil endpoint")
	}
	pm.specs = append(pm.specs,
		&ApplicationEndpoint{
			Name:        endpoint.Name,
			Service:     endpoint.Service,
			Application: endpoint.Application,
			Namespace:   endpoint.Namespace,
			Replicas:    endpoint.Replicas,
			Endpoint:    endpoint,
		})
	pm.ids[ToUnique(endpoint)]++
	return nil
}

func (pm *ServiceManager) Reserve(ctx context.Context) error {
	w := wool.Get(ctx).In("ServiceManager.Reserve")
	m, err := pm.strategy.Reserve(ctx, pm.host, pm.specs)
	if err != nil {
		return w.Wrapf(err, "cannot reserve ports")
	}
	pm.reserved = m
	return nil
}

// NetworkMapping returns the network mapping for the service to be passed back to codefly
func (pm *ServiceManager) NetworkMapping(context.Context) ([]*basev0.NetworkMapping, error) {
	w := wool.Get(context.Background()).In("ServiceManager.NetworkMapping")
	var nets []*basev0.NetworkMapping
	// A bit weird, replace here
	for _, instance := range pm.reserved.ApplicationEndpointInstances {
		address := instance.Address()
		unique := configurations.ServiceUnique(instance.ApplicationEndpoint.Application, instance.ApplicationEndpoint.Service)
		if url, ok := pm.external[unique]; ok {
			w.Debug("external", wool.Field("url", url))
			address = url
		}
		nets = append(nets, &basev0.NetworkMapping{
			Application: instance.ApplicationEndpoint.Application,
			Service:     instance.ApplicationEndpoint.Service,
			Endpoint:    instance.ApplicationEndpoint.Endpoint,
			Addresses:   []string{address},
		})
	}
	return nets, nil
}

func (pm *ServiceManager) ApplicationEndpointInstance(ctx context.Context, endpoint *basev0.Endpoint) (*ApplicationEndpointInstance, error) {
	w := wool.Get(ctx).In("ServiceManager.ApplicationEndpointInstance", wool.Field("endpoint", endpoint.Name))
	var result *ApplicationEndpointInstance
	for _, e := range pm.reserved.ApplicationEndpointInstances {
		if ToUnique(e.ApplicationEndpoint.Endpoint) == ToUnique(endpoint) {
			if result != nil {
				return nil, w.NewError("duplicated endpoint")
			}
			result = e
		}
	}
	return result, nil
}

func (pm *ServiceManager) Port(ctx context.Context, endpoint *basev0.Endpoint) (int, error) {
	w := wool.Get(ctx).In("ServiceManager.Port", wool.Field("endpoint", endpoint.Name))
	instance, err := pm.ApplicationEndpointInstance(ctx, endpoint)
	if err != nil {
		return 0, w.Wrapf(err, "cannot find endpoint")
	}
	return instance.Port, nil
}

func (pm *ServiceManager) ApplicationEndpointInstances() []*ApplicationEndpointInstance {
	return pm.reserved.ApplicationEndpointInstances
}

func (pm *ServiceManager) WithExternalDNS(data map[string]string) {
	pm.external = data
}
