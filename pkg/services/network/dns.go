package network

import (
	"context"
	"fmt"

	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
)

type DNS struct {
	Project        string
	OrganizationID string
}

func (dns *DNS) ToService(e *basev0.Endpoint) string {
	if dns.OrganizationID != "" {
		return fmt.Sprintf("%s.%s.%s.%s.svc.cluster.local", e.Service, e.Application, dns.Project, dns.OrganizationID)
	}
	return fmt.Sprintf("%s.%s.%s.svc.cluster.local", e.Service, e.Application, dns.Project)
}

func (dns *DNS) Reserve(_ context.Context, _ string, es []*ApplicationMapping) (*ApplicationEndpointInstances, error) {
	m := &ApplicationEndpointInstances{}
	for _, e := range es {
		port, err := configurations.Port(e.Endpoint.Api)
		if err != nil {
			return nil, err
		}
		m.ApplicationMappingInstances = append(m.ApplicationMappingInstances, &ApplicationEndpointInstance{
			ApplicationMapping: e,
			Port:               port,
			Host:               dns.ToService(e.Endpoint),
		})
	}
	return m, nil
}

func NewDNS(_ context.Context, project string) (*DNS, error) {
	return &DNS{Project: project}, nil
}

func (dns *DNS) WithOrganizationID(organizationID string) *DNS {
	dns.OrganizationID = organizationID
	return dns
}

func NewServiceDNSManager(_ context.Context, dns *DNS, endpoints ...*basev0.Endpoint) (*ServiceManager, error) {
	return &ServiceManager{
		endpoints: endpoints,
		strategy:  dns,
		ids:       make(map[string]int),
	}, nil
}
