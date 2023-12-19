package update

import (
	"context"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ProjectCmd represents the run command
var ProjectCmd = &cobra.Command{
	Use:   "project",
	Short: "Update a project",

	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.Background()

		provider := wool.New(ctx, configurations.CLI.AsResource())

		provider.WithLogger(common.CLI())
		defer provider.Done()

		ctx = provider.WithContext(ctx)

		project := common.Project(ctx)
		updateProject(ctx, project)

	},
}

func updateProject(ctx context.Context, project *configurations.Project) {
	apps, err := project.LoadApplications(ctx)
	cli.ExitOnError(err, "cannot load applications")
	for _, app := range apps {
		cli.Header(1, "Updating application <{{.Name}}>", app)
		svcs, err := app.LoadServices(ctx)
		cli.ExitOnError(err, "cannot load services")
		for _, svc := range svcs {
			cli.Header(2, "Updating service <{{.Name}}>", svc)
			err := services.UpdateAgent(ctx, svc)
			cli.ExitOnError(err, "cannot update service")
		}
	}
}
