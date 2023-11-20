package network

import (
	"fmt"
	"strings"

	corev1 "github.com/codefly-dev/cli/proto/v1/core"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

type Strategy interface {
	Reserve(host string, endpoints []ApplicationEndpoint) (*ApplicationEndpointInstances, error)
}

// An ApplicationEndpoint takes a service Endpoint
// and embed it so it can be used across the applications
type ApplicationEndpoint struct {
	Service     string
	Application string
	Namespace   string
	Endpoint    *corev1.Endpoint
	PortBinding string // something like 8080/tcp
}

func (e ApplicationEndpoint) Unique() string {
	return ToUnique(e.Endpoint)
}

func (e ApplicationEndpoint) Clone() ApplicationEndpoint {
	return ApplicationEndpoint{
		Service:     e.Service,
		Application: e.Application,
		Namespace:   e.Namespace,
		Endpoint:    e.Endpoint,
		PortBinding: e.PortBinding,
	}
}

// An ApplicationEndpointInstance is an instance of an ApplicationEndpoint
type ApplicationEndpointInstance struct {
	ApplicationEndpoint ApplicationEndpoint
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

func ToEndpoint(endpoint *corev1.Endpoint) *configurations.Endpoint {
	var api string
	switch endpoint.Api.Value.(type) {
	case *corev1.API_Grpc:
		api = configurations.Grpc
	case *corev1.API_Rest:
		api = configurations.Rest
	case *corev1.API_Tcp:
		api = configurations.Tcp
	}
	return &configurations.Endpoint{
		Name:        endpoint.Name,
		Description: endpoint.Description,
		Api:         api,
	}
}

func ToUnique(endpoint *corev1.Endpoint) string {
	return ToEndpoint(endpoint).Unique(endpoint.Application, endpoint.Service)
}

type Address struct {
	Host string
	Port int
}

func (pm *ApplicationEndpointInstances) Address(endpoint *corev1.Endpoint) *Address {
	// Returns the first one
	logger := shared.NewLogger("network.ApplicationEndpointInstances.Address")
	logger.TODO("implement the free local IP: will depend on deploy")
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
