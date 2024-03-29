package generators

import (
	"context"

	"github.com/codefly-dev/cli/pkg/services/services"
	"github.com/codefly-dev/core/companions/proto"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/configurations/languages"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/wool"
)

func GRPC(ctx context.Context, service *configurations.Service, language languages.Language, destination string) error {
	w := wool.Get(ctx).In("generateGRPCs", wool.ThisField(service))
	endpoints, err := getGRPCEndpoints(ctx, service)
	if err != nil {
		return w.Wrapf(err, "cannot get grpc endpoints")
	}
	err = proto.GenerateGRPC(ctx, language, destination, service.Name, endpoints...)
	if err != nil {
		return w.Wrapf(err, "cannot generate grpc")
	}
	return nil
}

func getGRPCEndpoints(ctx context.Context, service *configurations.Service) ([]*basev0.Endpoint, error) {
	w := wool.Get(ctx).In("getGRPCEndpoints")
	// Use the Builder
	instance, err := services.Load(ctx, service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load builder")
	}
	err = instance.LoadBuilder(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load builder")
	}
	res, err := instance.Builder.Load(ctx, configurations.LocalProto())
	if err != nil {
		return nil, w.Wrapf(err, "cannot load builder")
	}
	// filter-out gRPC
	var endpoints []*basev0.Endpoint
	for _, endpoint := range res.Endpoints {
		if grpc := configurations.IsGRPC(ctx, endpoint); grpc != nil {
			endpoints = append(endpoints, endpoint)
		}
	}
	w.Debug("got endpoints", wool.Field("endpoints", configurations.MakeManyEndpointSummary(endpoints)))
	return endpoints, nil
}
