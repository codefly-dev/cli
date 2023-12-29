package runner

//
//import (
//	"context"
//	"errors"
//	"fmt"
//	"strings"
//
//	"github.com/codefly-dev/core/agents/endpoints"
//	basev1 "github.com/codefly-dev/core/generated/go/base/v1"
//	"github.com/codefly-dev/core/wool"
//
//	"github.com/codefly-dev/core/configurations"
//	"github.com/codefly-dev/core/shared"
//)
//
//type EndpointHolder struct {
//	application string
//	endpoint    *basev1.Endpoint
//}
//
//func (p *EndpointHolder) AccessibleFrom(ctx context.Context, app string) bool {
//	w := wool.Get(ctx).In("applications.EndpointHolder.AccessibleFrom<%s>", p.endpoint.Name)
//	logger.Debuf("visibility of endpoint <%s>: <%s> | access from app %v", p.endpoint.Name, p.endpoint.Visibility, app)
//	if p.endpoint.Visibility == "" || p.endpoint.Visibility == "private" {
//		return p.application == app
//	}
//	return true
//}
//
//func (p *EndpointHolder) Add(endpoint *basev1.Endpoint) {
//	p.endpoint = endpoint
//}
//
//func NewEndpointHolder(application string, endpoint *basev1.Endpoint) *EndpointHolder {
//	return &EndpointHolder{application: application, endpoint: endpoint}
//}
//
//type ServiceEndpointManager struct {
//	application string
//	service     *configurations.Service
//	endpoints   []*EndpointHolder
//}
//
//func (s *ServiceEndpointManager) Add(ctx context.Context, endpoint *basev1.Endpoint) error {
//	w := wool.Get(ctx).In("applications.ServiceEndpointManager.Add<%s>", s.service.Unique())
//	logger.Debuf("adding endpoint: %s", endpoints.Destination(endpoint))
//	api, err := endpoints.WhichAPIFromEndpoint(endpoint)
//	if err != nil {
//		var nilApiError *endpoints.NilAPIError
//		if errors.As(err, &nilApiError) {
//			return logger.Wrapf(err, "got an empty api")
//		}
//		var unknownApiError *endpoints.UnknownAPIError
//		if errors.As(err, &unknownApiError) {
//			return logger.Wrapf(err, "got an unknown api")
//		}
//	}
//	for _, holder := range s.endpoints {
//		if holder.endpoint.Api == endpoint.Api {
//			return nil
//		}
//	}
//
//	logger.Debuf("adding endpoint: %s::%s", endpoint.Name, api)
//	s.endpoints = append(s.endpoints, NewEndpointHolder(s.application, endpoint))
//	return nil
//}
//
//func (s *ServiceEndpointManager) Get(ctx context.Context, ref *configurations.EndpointReference) (*basev1.Endpoint, error) {
//	w := wool.Get(ctx).In("applications.ServiceEndpointManager.Get<%s>", s.service.Name)
//	for _, holder := range s.endpoints {
//		if holder.endpoint.Name == ref.Name {
//			return holder.endpoint, nil
//		}
//	}
//	return nil, logger.Errorf("endpoint <%s> not found", ref.Name)
//}
//
//func (s *ServiceEndpointManager) ServiceGroupEndpoints(ctx context.Context, dep *configurations.ServiceDependency) (*basev1.ServiceEndpointGroup, error) {
//	w := wool.Get(ctx).In("applications.ServiceEndpointManager.ServiceGroupEndpoints<%s>", s.service.Name)
//	logger.TODO("visibility")
//	var es []*basev1.Endpoint
//	for _, holder := range s.endpoints {
//		// Visibility check
//		if !holder.AccessibleFrom(ctx, dep.Application) {
//			continue
//		}
//		es = append(es, holder.endpoint)
//	}
//	logger.Debuf("endpoints: #%d", len(es))
//	if len(es) > 0 {
//		return &basev1.ServiceEndpointGroup{
//			Name:      s.service.Name,
//			Endpoints: es,
//		}, nil
//	}
//	return &basev1.ServiceEndpointGroup{Name: dep.Name}, nil
//}
//
//func NewServiceEndpointManager(service *configurations.Service) *ServiceEndpointManager {
//	return &ServiceEndpointManager{
//		service:     service,
//		application: service.Application,
//	}
//}
//
//type ApplicationEndpointManager struct {
//	logger      shared.BaseLogger
//	services    []*ServiceEndpointManager
//	application *configurations.Application
//}
//
//func (m *ApplicationEndpointManager) Get(ctx context.Context, name string, ref *configurations.EndpointReference) (*basev1.Endpoint, error) {
//	w := wool.Get(ctx).In("applications.ApplicationEndpointManager.Get")
//	for _, svc := range m.services {
//		if svc.service.Name == name {
//			return svc.Get(ctx, ref)
//		}
//	}
//	return nil, logger.Errorf("service <%s> not found", name)
//}
//
//func (m *ApplicationEndpointManager) GetServiceEndpointManager(service *configurations.Service) (*ServiceEndpointManager, error) {
//	for _, svc := range m.services {
//		if svc.service.Unique() == service.Unique() {
//			return svc, nil
//		}
//	}
//	svc := NewServiceEndpointManager(service)
//	m.services = append(m.services, svc)
//	return svc, nil
//}
//
//func (m *ApplicationEndpointManager) ServiceEndpointManager(name string) (*ServiceEndpointManager, error) {
//	for _, svc := range m.services {
//		if svc.service.Name == name {
//			return svc, nil
//		}
//	}
//	return nil, m.logger.Errorf("service <%s> not found", name)
//}
//
//func (m *ApplicationEndpointManager) Add(ctx context.Context, service *configurations.Service, endpoints []*basev1.Endpoint) error {
//	w := wool.Get(ctx).In("applications.ApplicationEndpointManager.Add<%s>", service.Name)
//	for _, endpoint := range endpoints {
//		logger.Debuf("adding endpoint: %s | visibility <%s>", endpoint.Name, endpoint.Visibility)
//		svc, err := m.GetServiceEndpointManager(service)
//		if err != nil {
//			return err
//		}
//		err = svc.Add(ctx, endpoint)
//		if err != nil {
//			return err
//		}
//	}
//	return nil
//}
//
//func GetEndpoints(ctx context.Context, configuration *configurations.Service) ([]*basev1.Endpoint, error) {
//	w := wool.Get(ctx).In("applications.GetEndpointDependencyGroup<%s>", configuration.Name)
//	app, err := GetApplicationEndpointManager(ctx, configuration.Application)
//	if err != nil {
//		return nil, logger.Wrapf(err, "cannot get application endpoint manager")
//	}
//	svc, err := app.GetServiceEndpointManager(configuration)
//	if err != nil {
//		return nil, logger.Wrapf(err, "cannot get service endpoint manager")
//	}
//	var es []*basev1.Endpoint
//	for _, h := range svc.endpoints {
//		es = append(es, h.endpoint)
//	}
//	return es, nil
//}
//
//func GetEndpointDependencyGroup(ctx context.Context, service *configurations.Service) (*basev1.EndpointGroup, error) {
//	w := wool.Get(ctx).In("applications.GetEndpointDependencyGroup<%s>", service.Name)
//	// We want to find the dependencies for this service
//	target := &configurations.ServiceDependency{Name: service.Name, Application: service.Application}
//	logger.Debuf("looking in the endpoint manager dependencies for %s", target)
//	var groups []*basev1.ApplicationEndpointGroup
//	for _, dep := range service.Dependencies {
//		app, err := GetApplicationEndpointManager(ctx, dep.Application)
//		if err != nil {
//			return nil, logger.Wrapf(err, "cannot get application endpoint manager")
//		}
//		group, err := app.ApplicationGroupEndpoints(ctx, target)
//		if err != nil {
//			return nil, logger.Wrapf(err, "cannot get application group endpoints")
//		}
//		if group == nil {
//			continue
//		}
//		groups = append(groups, group)
//	}
//	if len(groups) > 0 {
//		return &basev1.EndpointGroup{
//			ApplicationEndpointGroup: groups,
//		}, nil
//	}
//	return nil, nil
//}
//
//func (m *ApplicationEndpointManager) ApplicationGroupEndpoints(ctx context.Context, dep *configurations.ServiceDependency) (*basev1.ApplicationEndpointGroup, error) {
//	var groups []*basev1.ServiceEndpointGroup
//	for _, svc := range m.services {
//		group, err := svc.ServiceGroupEndpoints(ctx, dep)
//		if err != nil {
//			return nil, m.logger.Wrapf(err, "cannot get service group endpoints")
//		}
//		if group == nil {
//			continue
//		}
//		groups = append(groups, group)
//	}
//	if len(groups) > 0 {
//		return &basev1.ApplicationEndpointGroup{
//			Name:                  m.application.Name,
//			ServiceEndpointGroups: groups,
//		}, nil
//	}
//	return nil, nil
//}
//
//var managers []*ApplicationEndpointManager
//
//func init() {
//}
//
//func GetApplicationEndpointManager(ctx context.Context, name string) (*ApplicationEndpointManager, error) {
//	w := wool.Get(ctx).In("applications.GetApplicationEndpointManager<%s>", name)
//	for _, manager := range managers {
//		if manager.application.Name == name {
//			return manager, nil
//		}
//	}
//	logger.Debuf("loading endpoint manager")
//	return LoadApplicationEndpointManager(name)
//}
//
//func LoadApplicationEndpointManager(name string) (*ApplicationEndpointManager, error) {
//	//w := wool.Get(ctx).In("applications.LoadApplicationEndpointManager<%s>", name)
//	//config, err := configurations.LoadApplicationFromName(name)
//	//if err != nil {
//	//	return nil, logger.Wrapf(err, "cannot load application")
//	//}
//	//app, err := Load(configurations.MustCurrentProject(), config, FactoryMode)
//	//if err != nil {
//	//	return nil, logger.Wrapf(err, "cannot load application")
//	//}
//	//err = app.FactoryInit()
//	//if err != nil {
//	//	return nil, logger.Wrapf(err, "cannot init application")
//	//}
//	//return app.EndpointManager, nil
//	return nil, nil
//}
//
//func NewApplicationEndpointManager(ctx context.Context, app *configurations.Application) *ApplicationEndpointManager {
//	mgr := &ApplicationEndpointManager{
//		application: app,
//		logger:      shared.GetLogger(ctx).With("applications.ApplicationEndpointManager<%s>", app.Name),
//	}
//	managers = append(managers, mgr)
//	return mgr
//}
//
//func ShowEndpointManagerState(ctx context.Context) {
//	w := wool.Get(ctx).In("applications.ShowEndpointManagerState")
//	var es []string
//	for _, manager := range managers {
//		es = append(es, fmt.Sprintf("- Application: %s", manager.application.Name))
//		for _, svc := range manager.services {
//			es = append(es, fmt.Sprintf("  - Service: %s", svc.service.Name))
//			for _, e := range svc.endpoints {
//				es = append(es, fmt.Sprintf("    - Endpoint: %s", endpoints.Destination(e.endpoint)))
//			}
//		}
//	}
//	logger.Debuf("state: \n%s", strings.Join(es, "\n"))
//
//}
