package services

import (
	"context"

	"github.com/codefly-dev/core/configurations"
)

func Run(ctx context.Context, service *configurations.Service) error {
	//w := wool.Get(ctx).In("service.Run")
	//instance, err := services.Load(ctx, service)
	//if err != nil {
	//	return logger.Wrapf(err, "cannot load service instance")
	//}
	//
	//if instance.Runtime == nil {
	//	cli.Error("No runtime is implemented for service <{{.Name}}>", service)
	//	return nil
	//}
	//init, err := instance.Runtime.Init(ctx)
	//if err != nil {
	//	return logger.Wrapf(err, "cannot init service instance")
	//}
	//logger.Debuf("init response: %+v", init)
	//conf, err := instance.Runtime.Configure(ctx, &runtimev1.ConfigureRequest{})
	//if err != nil {
	//	return logger.Wrapf(err, "cannot configure service instance")
	//}
	//logger.Debuf("configure response: %+v", conf)
	//start, err := instance.Runtime.Start(ctx, &runtimev1.StartRequest{})
	//if err != nil {
	//	return logger.Wrapf(err, "cannot start service instance")
	//}
	//logger.Debuf("start response: %+v", start)
	return nil
}
