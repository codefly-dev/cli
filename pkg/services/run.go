package services

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	runtimev1 "github.com/codefly-dev/core/generated/v1/go/proto/services/runtime"
	"github.com/codefly-dev/core/shared"
)

func Run(ctx context.Context, service *configurations.Service) error {
	logger := shared.GetLogger(ctx).With("service.Run")
	instance, err := services.Load(ctx, service)
	if err != nil {
		return logger.Wrapf(err, "cannot load service instance")
	}

	if instance.Runtime == nil {
		cli.Error("No runtime is implemented for service <{{.Name}}>", service)
		return nil
	}
	init, err := instance.Runtime.Init(ctx)
	if err != nil {
		return logger.Wrapf(err, "cannot init service instance")
	}
	logger.Debugf("init response: %+v", init)
	conf, err := instance.Runtime.Configure(ctx, &runtimev1.ConfigureRequest{})
	if err != nil {
		return logger.Wrapf(err, "cannot configure service instance")
	}
	logger.Debugf("configure response: %+v", conf)
	start, err := instance.Runtime.Start(ctx, &runtimev1.StartRequest{})
	if err != nil {
		return logger.Wrapf(err, "cannot start service instance")
	}
	logger.Debugf("start response: %+v", start)
	return nil
}
