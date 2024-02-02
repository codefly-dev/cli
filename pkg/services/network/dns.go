package network

import (
	"context"
	"fmt"

	"github.com/codefly-dev/core/configurations"

	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
)

type DNS struct{}

func ToDNS(e *basev0.Endpoint) string {
	return fmt.Sprintf("%s-%s.%s.svc.cluster.local", e.Service, e.Application, e.Namespace)
}

func (r DNS) Reserve(_ context.Context, _ string, es []*ApplicationEndpoint) (*ApplicationEndpointInstances, error) {
	m := &ApplicationEndpointInstances{}
	for _, e := range es {
		port, err := configurations.StandardPort(e.Endpoint.Api)
		if err != nil {
			return nil, err
		}
		m.ApplicationEndpointInstances = append(m.ApplicationEndpointInstances, &ApplicationEndpointInstance{
			ApplicationEndpoint: e,
			Port:                port,
			Host:                ToDNS(e.Endpoint),
		})
	}
	return m, nil
}

func NewServiceDNSManager(_ context.Context, endpoints ...*basev0.Endpoint) (*ServiceManager, error) {
	return &ServiceManager{
		endpoints: endpoints,
		strategy:  &DNS{},
		ids:       make(map[string]int),
	}, nil
}
