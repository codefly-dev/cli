package application

import (
	"fmt"

	"github.com/codefly-dev/cli/pkg/plugins/endpoints"
	corev1 "github.com/codefly-dev/cli/proto/v1/core"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

type EndpointHolder struct {
	//application *configurations.Application
	//service  *configurations.Service
	endpoint *corev1.Endpoint
}

func (p *EndpointHolder) Add(endpoint *corev1.Endpoint) {
	p.endpoint = endpoint
}

func NewEndpointHolder(endpoint *corev1.Endpoint) *EndpointHolder {
	return &EndpointHolder{endpoint: endpoint}
}

type ServiceEndpointManager struct {
	//application *configurations.Application
	service   *configurations.Service
	endpoints []*EndpointHolder
}

func (s *ServiceEndpointManager) Add(endpoint *corev1.Endpoint) error {
	logger := shared.NewLogger("applications.ServiceEndpointManager.Add<%s>", s.service.Unique())
	api, err := endpoints.WhichApiFromEndpoint(endpoint)
	if err != nil {
		return logger.Wrapf(err, "cannot determine api from endpoint")
	}
	for _, holder := range s.endpoints {
		if holder.endpoint.Api == endpoint.Api {
			return nil
		}
	}

	logger.Debugf("adding endpoint: %s::%s", endpoint.Name, api)
	s.endpoints = append(s.endpoints, NewEndpointHolder(endpoint))
	return nil
}

func (s *ServiceEndpointManager) Get(ref *configurations.EndpointReference) (*corev1.Endpoint, error) {
	logger := shared.NewLogger("applications.ServiceEndpointManager.Get<%s>", s.service.Name)
	for _, holder := range s.endpoints {
		if holder.endpoint.Name == ref.Name {
			return holder.endpoint, nil
		}
	}
	return nil, logger.Errorf("endpoint <%s> not found", ref.Name)
}

func (s *ServiceEndpointManager) ServiceGroupEndpoints(dep *configurations.ServiceDependency) (*corev1.ServiceEndpointGroup, error) {
	logger := shared.NewLogger("applications.ServiceEndpointManager.ServiceGroupEndpoints<%s>", s.service.Name)
	logger.TODO("visibility")
	var es []*corev1.Endpoint
	for _, holder := range s.endpoints {

		es = append(es, holder.endpoint)
	}
	logger.Debugf("endpoints: #%d", len(es))
	if len(es) > 0 {
		return &corev1.ServiceEndpointGroup{
			Name:      dep.Name,
			Endpoints: es,
		}, nil
	}
	return &corev1.ServiceEndpointGroup{Name: dep.Name}, nil
}

func (s *ServiceEndpointManager) ShowAll() {
	fmt.Println("showing", s.service.Name, len(s.endpoints))
	for _, endpoint := range s.endpoints {
		fmt.Printf("    Endpoint: %s \n", endpoint.endpoint.Name)
	}

}

func NewServiceEndpointManager(service *configurations.Service) *ServiceEndpointManager {
	return &ServiceEndpointManager{
		service: service,
	}
}

type ApplicationEndpointManager struct {
	logger      *shared.Logger
	services    []*ServiceEndpointManager
	application *configurations.Application
}

func (m *ApplicationEndpointManager) Get(name string, ref *configurations.EndpointReference) (*corev1.Endpoint, error) {
	logger := shared.NewLogger("applications.ApplicationEndpointManager.Get")
	for _, svc := range m.services {
		if svc.service.Name == name {
			return svc.Get(ref)
		}
	}
	return nil, logger.Errorf("service <%s> not found", name)
}

func (m *ApplicationEndpointManager) GetServiceEndpointManager(service *configurations.Service) (*ServiceEndpointManager, error) {
	for _, svc := range m.services {
		if svc.service.Unique() == service.Unique() {
			return svc, nil
		}
	}
	svc := NewServiceEndpointManager(service)
	m.services = append(m.services, svc)
	return svc, nil
}

func (m *ApplicationEndpointManager) ServiceEndpointManager(name string) (*ServiceEndpointManager, error) {
	for _, svc := range m.services {
		if svc.service.Name == name {
			return svc, nil
		}
	}
	return nil, m.logger.Errorf("service <%s> not found", name)
}

func (m *ApplicationEndpointManager) Add(service *configurations.Service, endpoints []*corev1.Endpoint) error {
	for _, endpoint := range endpoints {
		svc, err := m.GetServiceEndpointManager(service)
		if err != nil {
			return err
		}
		err = svc.Add(endpoint)
		if err != nil {
			return err
		}
	}
	return nil
}

func GetEndpoints(configuration *configurations.Service) ([]*corev1.Endpoint, error) {
	logger := shared.NewLogger("applications.GetEndpointDependencyGroup<%s>", configuration.Name)
	app, err := GetApplicationEndpointManager(configuration.Application)
	if err != nil {
		return nil, logger.Wrapf(err, "cannot get application endpoint manager")
	}
	svc, err := app.GetServiceEndpointManager(configuration)
	if err != nil {
		return nil, logger.Wrapf(err, "cannot get service endpoint manager")
	}
	var es []*corev1.Endpoint
	for _, h := range svc.endpoints {
		es = append(es, h.endpoint)
	}
	return es, nil
}

func GetEndpointDependencyGroup(configuration *configurations.Service) (*corev1.EndpointGroup, error) {
	logger := shared.NewLogger("applications.GetEndpointDependencyGroup<%s>", configuration.Name)
	// Right now, only do +1 dependencies
	var groups []*corev1.ApplicationEndpointGroup
	for _, dep := range configuration.Dependencies {
		app, err := GetApplicationEndpointManager(dep.Application)
		if err != nil {
			return nil, logger.Wrapf(err, "cannot get application endpoint manager")
		}
		group, err := app.ApplicationGroupEndpoints(dep)
		if err != nil {
			return nil, logger.Wrapf(err, "cannot get application group endpoints")
		}
		if group == nil {
			continue
		}
		groups = append(groups, group)
	}
	if len(groups) > 0 {
		return &corev1.EndpointGroup{
			ApplicationEndpointGroup: groups,
		}, nil
	}
	return nil, nil
}

func (m *ApplicationEndpointManager) ApplicationGroupEndpoints(dep *configurations.ServiceDependency) (*corev1.ApplicationEndpointGroup, error) {
	var groups []*corev1.ServiceEndpointGroup
	for _, svc := range m.services {
		group, err := svc.ServiceGroupEndpoints(dep)
		if err != nil {
			return nil, m.logger.Wrapf(err, "cannot get service group endpoints")
		}
		if group == nil {
			continue
		}
		groups = append(groups, group)
	}
	if len(groups) > 0 {
		return &corev1.ApplicationEndpointGroup{
			Name:                  m.application.Name,
			ServiceEndpointGroups: groups,
		}, nil
	}
	return nil, nil
}

var managers []*ApplicationEndpointManager

func init() {
}

func GetApplicationEndpointManager(name string) (*ApplicationEndpointManager, error) {
	for _, manager := range managers {
		if manager.application.Name == name {
			return manager, nil
		}
	}
	return nil, fmt.Errorf("api manager for application <%s> not found: probably need to run a partial", name)
}

func NewApplicationEndpointManager(app *configurations.Application) *ApplicationEndpointManager {
	mgr := &ApplicationEndpointManager{
		application: app,
		logger:      shared.NewLogger("applications.ApplicationEndpointManager<%s>", app.Name),
	}
	managers = append(managers, mgr)
	return mgr
}

func CondensedOutput(group *corev1.EndpointGroup) []string {
	if group == nil {
		return nil
	}
	var outs []string
	for _, appGroup := range group.ApplicationEndpointGroup {
		for _, svcGroup := range appGroup.ServiceEndpointGroups {
			if len(svcGroup.Endpoints) > 0 {
				outs = append(outs, fmt.Sprintf("%s/%s[#%d]", appGroup.Name, svcGroup.Name, len(svcGroup.Endpoints)))
			}
		}
	}
	return outs
}

func ShowAll() {
	for _, manager := range managers {
		fmt.Println("Application:", manager.application.Name)
		for _, svc := range manager.services {
			fmt.Println("    Service:", svc.service.Name)
			for _, endpoint := range svc.endpoints {
				fmt.Printf("        Endpoint: %s \n", endpoint.endpoint.Name)
			}
		}
	}
}
