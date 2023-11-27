package v1

import (
	"context"

	"github.com/codefly-dev/cli/pkg/services"
	basev1 "github.com/codefly-dev/core/proto/v1/go/base"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

func FlattenEndpoints(group *basev1.EndpointGroup) []*basev1.Endpoint {
	var endpoints []*basev1.Endpoint
	if group == nil {
		return endpoints
	}
	for _, app := range group.ApplicationEndpointGroup {
		for _, svc := range app.ServiceEndpointGroups {
			endpoints = append(endpoints, svc.Endpoints...)
		}
	}
	return endpoints
}

func FlattenRestRoutes(group *basev1.EndpointGroup) []*basev1.RestRoute {
	endpoints := FlattenEndpoints(group)
	var routes []*basev1.RestRoute
	for _, ep := range endpoints {
		if rest := ep.Api.GetRest(); rest != nil {
			routes = append(routes, rest.Routes...)
		}
	}
	return routes
}

func DetectNewRoutes(ctx context.Context, known []*configurations.RestRoute, group *basev1.EndpointGroup) []*configurations.RestRoute {
	logger := ctx.Value(shared.Agent).(shared.BaseLogger)
	if group == nil {
		logger.Debugf("we have a nil group")
		return nil
	}
	logger.Debugf("application groups: #%d", len(group.ApplicationEndpointGroup))
	var newRoutes []*configurations.RestRoute
	for _, app := range group.ApplicationEndpointGroup {
		logger.DebugMe("service groups: %s #%d", app.Name, len(app.ServiceEndpointGroups))
		for _, svc := range app.ServiceEndpointGroups {
			logger.DebugMe("endpoints: %s #%d", svc.Name, len(svc.Endpoints))
			for _, ep := range svc.Endpoints {
				if rest := IsRest(ctx, ep.Api); rest != nil {
					for _, route := range rest.Routes {
						potential := &configurations.RestRoute{
							Application: app.Name,
							Service:     svc.Name,
							Path:        route.Path,
							Methods:     services.ConvertMethods(route.Methods),
						}
						if !containsRoute(ctx, known, potential) {
							newRoutes = append(newRoutes, potential)
						}
					}
				}
			}
		}
	}
	return newRoutes
}

func IsRest(ctx context.Context, api *basev1.API) *basev1.RestAPI {
	if api == nil {
		return nil
	}
	switch v := api.Value.(type) {
	case *basev1.API_Rest:
		return v.Rest
	default:
		return nil
	}
}

func containsRoute(ctx context.Context, known []*configurations.RestRoute, potential *configurations.RestRoute) bool {
	for _, k := range known {
		if k.Application == potential.Application && k.Service == potential.Service && k.Path == potential.Path {
			return true
		}
	}
	return false
}
