package generators

import (
	"context"

	"github.com/codefly-dev/core/companions/proto"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/languages"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/wool"
)

func GRPC(ctx context.Context, module *resources.Module, service *resources.Service, language languages.Language, destination string) error {
	w := wool.Get(ctx).In("generateGRPCs", wool.ThisField(resources.WithUnique(service)))
	endpoints, err := getGRPCEndpoints(ctx, module, service)
	if err != nil {
		return w.Wrapf(err, "cannot get grpc endpoints")
	}
	err = proto.GenerateGRPC(ctx, language, destination, service.Name, endpoints...)
	if err != nil {
		return w.Wrapf(err, "cannot generate grpc")
	}
	return nil
}

func getGRPCEndpoints(ctx context.Context, module *resources.Module, service *resources.Service) ([]*basev0.Endpoint, error) {
	w := wool.Get(ctx).In("getGRPCEndpoints")
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
	// filter-out gRPC
	var endpoints []*basev0.Endpoint
	for _, endpoint := range res.Endpoints {
		if grpc := resources.IsGRPC(ctx, endpoint); grpc != nil {
			endpoints = append(endpoints, endpoint)
		}
	}
	w.Debug("got endpoints", wool.Field("endpoints", resources.MakeManyEndpointSummary(endpoints)))
	return endpoints, nil
}
