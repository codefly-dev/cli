package add

import (
	"fmt"
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/actions/actions"
	actionsproject "github.com/codefly-dev/core/actions/project"
	"github.com/codefly-dev/core/configurations"
	"github.com/spf13/cobra"
)

// ProjectCmd represents the run command
var ProjectCmd = &cobra.Command{
	Use:   "project",
	Short: "Add an project",

	Run: func(cmd *cobra.Command, args []string) {

		if interactive {
			cli.Error("Interactive mode not implemented yet")
			cli.Exit()
		}
		if len(args) == 1 {
			name := args[0]
			newProject(name)
		}
	},
}

func newProject(name string) {
	ctx, done := common.NewContext()
	defer done()

	workspace := common.Workspace(ctx)

	if workspace.ExistsProject(name) {
		cli.Error("Project <%s> already exists", name)
		os.Exit(0)
	}
	confirm := models.Confirm(fmt.Sprintf("Do you want to add project <%s> in the current folder?", name), true)
	if !confirm {
		cli.Header(2, "Received loud and clear!")
	}
	cur, err := os.Getwd()
	cli.ExitOnError(err, "Cannot get current directory")
	action, err := actionsproject.NewActionAddProject(ctx, &actionsproject.AddProject{
		Workspace: workspace.Name,
		Name:      name,
		Path:      cur,
	})
	cli.ExitOnError(err, "cannot create action")
	out, err := actions.Run(ctx, action)
	if err != nil {
		cli.ExitOnError(err, "cannot add project")
	}
	project, err := actions.As[configurations.Project](out)
	if err != nil {
		cli.ExitOnError(err, "cannot add project")
	}
	cli.Header(2, "Project <%s> added and is now active", project.Name)
}

func init() {
	ProjectCmd.PersistentFlags().BoolVarP(&interactive, "interactive", "i", false, "interactive mode")
}
