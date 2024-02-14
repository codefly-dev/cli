package delete

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/spf13/cobra"
)

// ProjectCmd represents the run command
var ProjectCmd = &cobra.Command{
	Use:   "project",
	Short: "Delete an project",

	Run: func(cmd *cobra.Command, args []string) {
		if len(args) != 1 {
			cli.Error("You must provide a name for the project as the single argument")
			cli.SuccessExit()
		}
		name := args[0]
		deleteProject(name)
	},
}

func deleteProject(name string) {
	ctx, done := common.NewContext()
	defer done()

	workspace := common.Workspace(ctx)
	if !workspace.ExistsProject(name) {
		cli.Error("Project <%s>> does not exist in workspace", name)
		return
	}
	confirm := models.Confirm(ctx, fmt.Sprintf("Delete the project <%s>?", name), false)
	if confirm {
		err := workspace.DeleteProject(ctx, name)
		cli.ExitOnError(err, "cannot delete project")
		cli.Header(2, "Project <%s> deleted", name)
	} else {
		cli.Header(2, "Abort! Heard loud and clear.")
	}
}
