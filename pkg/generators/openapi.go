package generators

import (
	"context"

	"github.com/codefly-dev/core/companions/proto"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/languages"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
)

func OpenAPI(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, language languages.Language, destination string) error {
	w := wool.Get(ctx).In("generateOpenAPIs", wool.ThisField(resources.WithUnique(service)))
	endpoints, err := getOpenAPIEndpoints(ctx, module, service)
	if err != nil {
		return w.Wrapf(err, "cannot get OpenAPI endpoints")
	}
	err = proto.GenerateOpenAPI(ctx, language, destination, service.Name, endpoints...)
	if err != nil {
		return w.Wrapf(err, "cannot generate OpenAPI")
	}
	return nil
}

func getOpenAPIEndpoints(ctx context.Context, module *resources.Module, service *resources.Service) ([]*basev0.Endpoint, error) {
	w := wool.Get(ctx).In("getOpenAPIEndpoints")
	// Use the Builder
	instance, err := services.Load(ctx, module, service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load builder")
	}
	err = instance.LoadBuilder(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load builder")
	}
	res, err := instance.Builder.Load(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load builder")
	}
	// filter-out OpenAPI
	var endpoints []*basev0.Endpoint
	for _, endpoint := range res.Endpoints {
		if rest := resources.IsRest(ctx, endpoint); rest != nil {
			endpoints = append(endpoints, endpoint)
		}
	}
	w.Debug("got endpoints", wool.Field("endpoints", resources.MakeManyEndpointSummary(endpoints)))
	return endpoints, nil
}
