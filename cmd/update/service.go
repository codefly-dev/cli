package update

import (
	"context"

	"github.com/codefly-dev/cli/pkg/services/services"
	"github.com/codefly-dev/core/resources"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Update a service",

	Run: func(cmd *cobra.Command, args []string) {
		service := common.Service(context.Background())
		updateService(service)
	},
}

func updateService(service *resources.Service) {
	ctx, done := common.NewContext()
	defer done()

	svc := common.Service(ctx)

	cli.Header(2, "Updating service <%s>", svc.Name)
	update, err := services.UpdateAgent(ctx, svc)
	cli.ExitOnError(err, "cannot update service")
	if update.AgentUpdate != nil {
		cli.Header(2, "Updating agent <%s> version: %s -> %s", update.AgentUpdate.Name, update.AgentUpdate.From, update.AgentUpdate.To)
	}
}
