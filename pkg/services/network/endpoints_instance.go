package network

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/core/configurations/standards"

	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
)

type Strategy interface {
	Reserve(ctx context.Context, host string, endpoints []*ApplicationEndpoint) (*ApplicationEndpointInstances, error)
}

// An ApplicationEndpoint takes a service Endpoint
// and embed it so it can be used across the applications
type ApplicationEndpoint struct {
	Name        string
	Service     string
	Application string
	Namespace   string
	Endpoint    *basev0.Endpoint
	PortBinding string // something like 8080/tcp
}

func (e ApplicationEndpoint) Unique() string {
	return ToUnique(e.Endpoint)
}

func (e ApplicationEndpoint) Clone() ApplicationEndpoint {
	return ApplicationEndpoint{
		Name:        e.Name,
		Service:     e.Service,
		Application: e.Application,
		Namespace:   e.Namespace,
		Endpoint:    e.Endpoint,
		PortBinding: e.PortBinding,
	}
}

// An ApplicationEndpointInstance is an instance of an ApplicationEndpoint
type ApplicationEndpointInstance struct {
	ApplicationEndpoint *ApplicationEndpoint
	Port                int
	Host                string
}

func (m *ApplicationEndpointInstance) Name() string {
	return strings.ToLower(m.ApplicationEndpoint.Service)
}

func (m *ApplicationEndpointInstance) Address() string {
	return fmt.Sprintf("%s:%d", m.Host, m.Port)
}

func (m *ApplicationEndpointInstance) StringPort() string {
	return fmt.Sprintf("%d", m.Port)
}

type ApplicationEndpointInstances struct {
	ApplicationEndpointInstances []*ApplicationEndpointInstance
}

func (pm *ApplicationEndpointInstances) First() *ApplicationEndpointInstance {
	return pm.ApplicationEndpointInstances[0]
}

func ToEndpoint(endpoint *basev0.Endpoint) *configurations.Endpoint {
	var api string
	switch endpoint.Api.Value.(type) {
	case *basev0.API_Grpc:
		api = standards.GRPC
	case *basev0.API_Rest:
		api = standards.REST
	case *basev0.API_Tcp:
		api = standards.TCP
	}
	return &configurations.Endpoint{
		Name:        endpoint.Name,
		Service:     endpoint.Service,
		Application: endpoint.Application,
		Description: endpoint.Description,
		API:         api,
	}
}

func ToUnique(endpoint *basev0.Endpoint) string {
	return ToEndpoint(endpoint).Unique()
}

type Address struct {
	Host string
	Port int
}

func (pm *ApplicationEndpointInstances) Address(endpoint *basev0.Endpoint) *Address {
	// Returns the first one
	for _, e := range pm.ApplicationEndpointInstances {
		if ToUnique(e.ApplicationEndpoint.Endpoint) == ToUnique(endpoint) {
			return &Address{
				Host: e.Host,
				Port: e.Port,
			}
		}
	}
	return nil
}
