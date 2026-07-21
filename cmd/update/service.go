package update

import (
	"fmt"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Update the active service to its latest compatible agent",

	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		service, err := common.LoadService(cmd.Context())
		if err != nil {
			return fmt.Errorf("cannot load service: %w", err)
		}
		return updateService(service)
	},
}

func updateService(service *resources.Service) error {
	ctx, done := common.NewContext()
	defer done()

	cli.Header(2, "Updating service <%s>", service.Name)
	update, err := services.UpdateAgent(ctx, service)
	if err != nil {
		return fmt.Errorf("cannot update service: %w", err)
	}
	if update.AgentUpdate != nil {
		cli.Header(2, "Updating agent <%s> version: %s -> %s", update.AgentUpdate.Name, update.AgentUpdate.From, update.AgentUpdate.To)
	}
	return nil
}
