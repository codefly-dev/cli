package generators

import (
	"context"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
)

// LoadServiceEndpoints loads a service's builder and returns every endpoint it
// reports at Builder.Load, api_details included.
func LoadServiceEndpoints(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) ([]*basev0.Endpoint, error) {
	w := wool.Get(ctx).In("LoadServiceEndpoints")
	instance, err := services.Load(ctx, workspace, module, service)
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
	return res.Endpoints, nil
}
