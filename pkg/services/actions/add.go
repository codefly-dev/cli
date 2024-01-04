package actions

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/glamour"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/services"
	"github.com/codefly-dev/core/actions/actions"
	actionservice "github.com/codefly-dev/core/actions/service"
	"github.com/codefly-dev/core/configurations"
	agentv1 "github.com/codefly-dev/core/generated/go/services/agent/v1"
	factoryv1 "github.com/codefly-dev/core/generated/go/services/factory/v1"

	"github.com/codefly-dev/core/wool"
)

func Add(ctx context.Context, input *actionservice.AddService) error {
	w := wool.Get(ctx).In("services.Add")
	action, err := actionservice.NewActionAddService(ctx, input)
	if err != nil {
		return w.Wrapf(err, "cannot create action")
	}

	out, err := actions.Run(ctx, action)
	if err != nil {
		return w.Wrapf(err, "cannot add service")
	}
	service, err := actions.As[configurations.Service](out)
	if err != nil {
		return w.Wrapf(err, "cannot add service")
	}

	cli.Header(2, "Service <{{.Name}}> added and is now active", service)

	instance, err := services.Load(ctx, service)
	if err != nil {
		return w.Wrapf(err, "cannot load service instance")
	}

	info, err := instance.Agent.GetAgentInformation(ctx, &agentv1.AgentInformationRequest{})
	if err != nil {

	}
	// README
	rendered, err := glamour.Render(info.ReadMe, "dark")
	if err != nil {
		return w.Wrapf(err, "cannot render info README")
	}
	// Paginate if long
	if len(strings.Split(rendered, "\n")) > 10 {
		cli.Paginate(rendered)
	} else {
		fmt.Println(rendered)
	}

	if instance.Factory == nil {
		cli.Header(2, "🎉 We are done!", service)
		return nil
	}
	_, err = instance.Factory.Load(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot create service instance")
	}

	_, err = instance.Factory.Create(ctx, &factoryv1.CreateRequest{})
	if err != nil {
		return w.Wrapf(err, "cannot create service instance")

	}
	return nil
}
