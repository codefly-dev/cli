package delete

import (
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/golor"
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

	w := common.Workspace(ctx)
	if !w.ExistsProject(name) {
		cli.Error("Project <{{.}}> does not exist in workspace", name)
		return
	}
	confirm := models.Confirm(golor.Sprintf("Delete the project <{{.}}>?", name), false)
	if confirm {
		err := w.DeleteProject(ctx, name)
		cli.ExitOnError(err, "cannot delete project")
		cli.Header(2, "Project <{{.}}> deleted", name)
	}
}
