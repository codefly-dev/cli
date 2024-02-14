package actions

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/cli/pkg/services/services"

	"github.com/charmbracelet/glamour"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/actions/actions"
	actionservice "github.com/codefly-dev/core/actions/service"
	"github.com/codefly-dev/core/configurations"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"

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

	cli.Header(2, "Service <%s> added and is now active", service.Name)

	instance, err := services.Load(ctx, service)
	if err != nil {
		return w.Wrapf(err, "cannot load service instance")
	}

	err = instance.LoadBuilder(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot load service instance")
	}

	info, err := instance.Agent.GetAgentInformation(ctx, &agentv0.AgentInformationRequest{})
	if err != nil {

	}
	// README
	rendered, err := glamour.Render(info.ReadMe, "dark")
	if err != nil {
		return w.Wrapf(err, "cannot render info README")
	}
	// Paginate if long
	if len(strings.Split(rendered, "\n")) > 50 {
		cli.Paginate(rendered)
	} else {
		fmt.Println(rendered)
	}

	_, err = instance.Builder.Load(ctx, configurations.LocalProto())
	if err != nil {
		return w.Wrapf(err, "cannot create service instance")
	}

	_, err = instance.Builder.Create(ctx, &builderv0.CreateRequest{})
	if err != nil {
		return w.Wrapf(err, "cannot create service instance")

	}
	return nil
}
