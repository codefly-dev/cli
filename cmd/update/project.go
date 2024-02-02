package update

import (
	"context"

	"github.com/codefly-dev/cli/pkg/services/services"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/configurations"
	"github.com/spf13/cobra"
)

// ProjectCmd represents the run command
var ProjectCmd = &cobra.Command{
	Use:   "project",
	Short: "Update a project",

	Run: func(cmd *cobra.Command, args []string) {
		project := common.Project(context.Background())
		updateProject(project)
	},
}

func updateProject(project *configurations.Project) {
	ctx, done := common.NewContext()
	defer done()

	apps, err := project.LoadApplications(ctx)
	cli.ExitOnError(err, "cannot load applications")
	for _, app := range apps {
		cli.Header(1, "Updating application <%s>", app.Name)
		svcs, err := app.LoadServices(ctx)
		cli.ExitOnError(err, "cannot load services")
		for _, svc := range svcs {
			cli.Header(2, "Updating service <%s>", svc.Name)
			update, err := services.UpdateAgent(ctx, svc)
			cli.ExitOnError(err, "cannot update service")
			if update.AgentUpdate != nil {
				cli.Header(2, "Updating agent <%s> version: %s -> %s", update.AgentUpdate.Name, update.AgentUpdate.From, update.AgentUpdate.To)
			}
		}
	}
}
