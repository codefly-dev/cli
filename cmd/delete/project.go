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
			cli.Exit()
		}
		name := args[0]
		deleteProject(name)
	},
}

func deleteProject(name string) {
	ctx, done := common.NewContext()
	defer done()

	workspace := common.Workspace(ctx)
	if workspace == nil {
		cli.Header(2, "Nothing to do. Just rm -rf .")
		return
	}
	if workspace.HasProject(name) {
		cli.Header(2, "Nothing to do. Just rm -rf .")
		return
	}
	confirm := models.Confirm(ctx, fmt.Sprintf("Delete the project from workspace <%s>?", name), false)
	if confirm {
		err := workspace.DeleteProject(ctx, name)
		cli.ExitOnError(err, "cannot delete project")
		cli.Header(2, "Project <%s> deleted", name)
	} else {
		cli.Header(2, "Abort! Heard loud and clear.")
	}
}
