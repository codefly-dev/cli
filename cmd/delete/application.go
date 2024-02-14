package delete

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/spf13/cobra"
)

// ApplicationCmd represents the run command
var ApplicationCmd = &cobra.Command{
	Use:   "application",
	Short: "Delete an application",

	Run: func(cmd *cobra.Command, args []string) {
		if len(args) != 1 {
			cli.Error("You must provide a name for the application as the single argument")
			cli.SuccessExit()
		}
		name := args[0]
		deleteApplication(name)
	},
}

func deleteApplication(name string) {
	ctx, done := common.NewContext()
	defer done()

	workspace := common.Workspace(ctx)
	project := common.Project(ctx)

	if !project.ExistsApplication(name) {
		cli.Error("Application <%s> does not exist in project <%s>", name, project)
		return
	}
	confirm := models.Confirm(ctx, fmt.Sprintf("Confirm deletion of application <%s> in project <%s>?", name, project.Name), false)
	if confirm {
		err := project.DeleteApplication(ctx, name)
		cli.ExitOnError(err, "cannot delete application")
		err = workspace.DeleteApplication(ctx, project.Name, name)
		cli.ExitOnError(err, "cannot delete application from workspace")
		cli.Header(2, "Application <%s> deleted!", name)
	} else {
		cli.Header(2, "Abort! Heard loud and clear.")
	}
}
