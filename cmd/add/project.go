package add

import (
	"context"
	"fmt"
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/actions/actions"
	actionsproject "github.com/codefly-dev/core/actions/project"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// ProjectCmd represents the run command
var ProjectCmd = &cobra.Command{
	Use:   "project",
	Short: "Add an project",

	Run: func(cmd *cobra.Command, args []string) {
		ctx := shared.NewContext()
		if interactive {
			cli.Error("Interactive mode not implemented yet")
			cli.Exit()
		}
		if len(args) == 1 {
			newProject(ctx, args[0])
		} else {
			// Find in Path and add to workspace
			dir, err := configurations.FindUp[configurations.Project](ctx)
			cli.ExitOnError(err, "Cannot find project in path")
			if dir == nil {
				cli.Error("No project found in path")
			}
			addProject(ctx, *dir)

		}
	},
}

func addProject(ctx context.Context, dir string) {
	project, err := configurations.LoadProjectFromDirUnsafe(ctx, dir)
	cli.ExitOnError(err, "Cannot load project from dir")
	workspace := common.Workspace(ctx)
	if workspace.ExistsProject(project.Name) {
		cli.Error("Project <{{.}}> already exists", project.Name)
		cli.Exit()
	}
	confirm := models.Confirm(fmt.Sprintf("Do you want to add project <%s> to your workspace?", project.Name), true)
	if !confirm {
		cli.Header(2, "Received loud and clear!")
	}
	err = workspace.AddProject(ctx, project)
	cli.ExitOnError(err, "Cannot add project to workspace")
	cli.Header(2, "Project <{{.Name}}> added to workspace", project)

}

func newProject(ctx context.Context, name string) {
	workspace := common.Workspace(ctx)

	if workspace.ExistsProject(name) {
		cli.Error("Project <{{.}}> already exists", name)
		os.Exit(0)
	}

	// Asks for Description
	addDescription := models.Confirm("Do you want to add a short description?", false)
	var desc string
	if addDescription {
		desc = models.Input("Description", "")
	}
	action, err := actionsproject.NewActionAddProject(ctx, &actionsproject.AddProject{
		Workspace:   workspace.Name,
		Name:        name,
		Description: desc,
	})
	shared.UnexpectedExitOnError(err, "cannot create action")
	out, err := actions.Run(ctx, action)
	if err != nil {
		shared.UnexpectedExitOnError(err, "cannot add project")
	}
	project, err := actions.As[configurations.Project](out)
	if err != nil {
		shared.UnexpectedExitOnError(err, "cannot add project")
	}
	cli.Header(2, "Project <{{.Name}}> added and is now active", project)
}

func init() {
	ProjectCmd.PersistentFlags().BoolVarP(&interactive, "interactive", "i", false, "interactive mode")
}
