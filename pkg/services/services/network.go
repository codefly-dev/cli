package services

import (
	"context"

	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/wool"
)

// NetworkMappingForRestRouteGroup finds the proper network mapping for a given route group
func NetworkMappingForRestRouteGroup(ctx context.Context, group *configurations.RestRouteGroup, mappings []*basev0.NetworkMapping) (*basev0.NetworkMapping, error) {
	w := wool.Get(ctx).In("services.NetworkMappingForRoute")
	for _, m := range mappings {
		if rest := m.Endpoint.Api.GetRest(); rest != nil {
			if m.Application == group.Application || m.Service == group.Service {
				return m, nil
			}
		}
	}
	return nil, w.NewError("cannot find network mapping for route <%v>", group)
}
