package create

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
	Short: "Create a new project",

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
	addToWorkspace := workspace != nil

	if workspace != nil && workspace.HasProject(name) {
		cli.Warning("Project <%s> already exists in workspace, won't be added to workspace", name)
		addToWorkspace = false
	}

	confirm := models.Confirm(ctx, fmt.Sprintf("codefly will create a project <%s> in the current folder. Proceed?", name), true)
	if !confirm {
		cli.Header(2, "Received loud and clear!")
	}
	cur, err := os.Getwd()
	cli.ExitOnError(err, "Cannot get current directory")

	var action actions.Action
	action, err = actionsproject.NewActionNewProject(ctx, &actionsproject.AddProject{
		Name: name,
		Path: cur,
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

	cli.Header(2, "Project <%s> created in current directory", project.Name)
	if addToWorkspace {
		action, err = actionsproject.NewActionAddProjectToWorkspace(ctx, &actionsproject.AddProjectToWorkspace{
			Workspace: workspace.Name,
			Name:      name,
			Path:      project.Dir(),
		})
		cli.ExitOnError(err, "cannot create action")

		out, err = actions.Run(ctx, action)
		if err != nil {
			cli.ExitOnError(err, "cannot add project to workspace")
		}
		cli.Header(2, "Project <%s> added to workspace", project.Name)
		// Make active
		action, err = actionsproject.NewActionSetProjectActive(ctx, &actionsproject.SetProjectActive{
			Workspace: workspace.Name,
			Name:      name,
		})
		cli.ExitOnError(err, "cannot create action")
		out, err = actions.Run(ctx, action)
		if err != nil {
			cli.ExitOnError(err, "cannot set project active")
		}
		cli.Header(2, "Project <%s> is now active", project.Name)

	}

	//_, err = providers.New(ctx, project)
	//cli.ExitOnError(err, "cannot create provider")

	//// Setup Version Control
	//err = deployment.InitRepository(ctx, project)
	//cli.ExitOnError(err, "cannot initialize repository")

}

func init() {
	ProjectCmd.PersistentFlags().BoolVarP(&interactive, "interactive", "i", false, "interactive mode")
}
