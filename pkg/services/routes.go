package services

import (
	"context"
	"fmt"

	"github.com/codefly-dev/core/agents/services"

	"github.com/codefly-dev/core/configurations"
	basev1 "github.com/codefly-dev/core/proto/v1/go/base"
	runtimev1 "github.com/codefly-dev/core/proto/v1/go/services/runtime"
)

func DetectNewRoutes(known []*configurations.RestRoute, routes []*configurations.RestRoute) []*configurations.RestRoute {
	var rs []*configurations.RestRoute
	for _, r := range routes {
		if !containsRoute(known, r) {
			rs = append(rs, r)
		}
	}
	return rs
}

func containsRoute(routes []*configurations.RestRoute, r *configurations.RestRoute) bool {
	for _, route := range routes {
		if route.Application == r.Application && route.Service == r.Service && route.Path == r.Path {
			return true
		}
	}
	return false
}

func ConvertRoutes(routes []*basev1.RestRoute, app string, service string) []*configurations.RestRoute {
	var rs []*configurations.RestRoute
	for _, r := range routes {
		rs = append(rs, &configurations.RestRoute{
			Path:        r.Path,
			Methods:     ConvertMethods(r.Methods),
			Application: app,
			Service:     service,
		})
	}
	return rs
}

func ConvertMethods(methods []basev1.HttpMethod) []configurations.HttpMethod {
	var ms []configurations.HttpMethod
	for _, m := range methods {
		ms = append(ms, ConvertMethod(m))
	}
	return ms
}

func ConvertMethod(m basev1.HttpMethod) configurations.HttpMethod {
	switch m {
	case basev1.HttpMethod_GET:
		return configurations.HttpMethodGet
	case basev1.HttpMethod_POST:
		return configurations.HttpMethodPost
	case basev1.HttpMethod_PUT:
		return configurations.HttpMethodPut
	case basev1.HttpMethod_DELETE:
		return configurations.HttpMethodDelete
	case basev1.HttpMethod_PATCH:
		return configurations.HttpMethodPatch
	case basev1.HttpMethod_OPTIONS:
		return configurations.HttpMethodOptions
	case basev1.HttpMethod_HEAD:
		return configurations.HttpMethodHead
	}
	panic(fmt.Sprintf("unknown http method: <%v>", m))
}

// NetworkMappingForRoute finds the proper network mapping for a given route
func NetworkMappingForRoute(ctx context.Context, route *configurations.RestRoute, mappings []*runtimev1.NetworkMapping) (*runtimev1.NetworkMapping, error) {
	logger := services.AgentLogger(ctx)
	for _, m := range mappings {
		if rest := m.Endpoint.Api.GetRest(); rest != nil {
			for _, r := range rest.Routes {
				if r.Path == route.Path {
					logger.TODO("METHODS AS WELL")
					return m, nil
				}
			}
			if m.Application == route.Application && m.Service == route.Service {
				return m, nil
			}
		}
	}
	return nil, logger.Errorf("cannot find network mapping for route <%s>", route)
}
