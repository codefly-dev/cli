package services

import (
	"context"

	communicate2 "github.com/codefly-dev/cli/pkg/cli/communicate"

	"github.com/charmbracelet/glamour"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/actions/actions"
	actionservice "github.com/codefly-dev/core/actions/service"
	"github.com/codefly-dev/core/agents/communicate"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	factoryv1 "github.com/codefly-dev/core/generated/v1/go/proto/services/factory"
	"github.com/codefly-dev/core/shared"
)

func Add(ctx context.Context, input *actionservice.AddService) error {
	logger := shared.GetLogger(ctx).With("service.Add")
	action, err := actionservice.NewActionAddService(ctx, input)
	if err != nil {
		return logger.Wrapf(err, "cannot create action")
	}

	out, err := actions.Run(ctx, action)
	if err != nil {
		return logger.Wrapf(err, "cannot add service")
	}
	service, err := actions.As[configurations.Service](out)
	if err != nil {
		return logger.Wrapf(err, "cannot add service")
	}

	cli.Header(2, "Service <{{.Name}}> added and is now active", service)

	instance, err := services.Load(ctx, service)
	if err != nil {
		return logger.Wrapf(err, "cannot load service instance")
	}

	if instance.Factory == nil {
		cli.Header(2, "🎉 We are done!", service)
		return nil
	}
	cli.Header(2, "Service <{{.Name}}> has a factory", service)
	init, err := instance.Factory.Init(ctx)
	if err != nil {
		return logger.Wrapf(err, "cannot create service instance")
	}
	// README
	rendered, err := glamour.Render(init.ReadMe, "dark")
	if err != nil {
		return logger.Wrapf(err, "cannot render readme")
	}
	cli.Paginate(rendered)

	// Communicate always
	err = communicate.Do[factoryv1.CreateRequest](ctx, instance.Factory, communicate2.NewPrompt())
	if err != nil {
		return logger.Wrapf(err, "cannot communicate")
	}

	_, err = instance.Factory.Create(ctx)
	if err != nil {
		return logger.Wrapf(err, "cannot create service instance")

	}
	return nil
}
