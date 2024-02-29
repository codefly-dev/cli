package add

import (
	"fmt"
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/actions/actions"
	actionsapplication "github.com/codefly-dev/core/actions/application"
	"github.com/codefly-dev/core/configurations"
	"github.com/spf13/cobra"
)

// ApplicationCmd represents the run command
var ApplicationCmd = &cobra.Command{
	Use:   "application",
	Short: "Add an application",

	Run: func(cmd *cobra.Command, args []string) {

		if interactive {
			cli.Error("Interactive mode not implemented yet")
			cli.Exit()
		}
		if len(args) != 1 {
			cli.Error("You must provide a name for the application as the single argument")
			return
		}
		name := args[0]
		addApplication(name)
	},
}

func addApplication(name string) {
	ctx, done := common.NewContext()
	defer done()

	workspace := common.Workspace(ctx)
	project := common.Project(ctx)

	if project.ExistsApplication(name) {
		cli.Error("Application <%s> already exists", name)
		os.Exit(0)
	}

	confirm := models.Confirm(ctx, fmt.Sprintf("Add an application in your project <%s>?", project.Name), true)
	if !confirm {
		cli.Header(2, "Received loud and clear!")
		os.Exit(0)
	}

	var action actions.Action
	var err error

	action, err = actionsapplication.NewActionAddApplication(ctx, &actionsapplication.AddApplication{
		Name:        name,
		ProjectPath: project.Dir(),
	})
	cli.ExitOnError(err, "cannot create action")
	out, err := actions.Run(ctx, action)
	if err != nil {
		cli.ExitOnError(err, "cannot add application")
	}
	app, err := actions.As[configurations.Application](out)
	if err != nil {
		cli.ExitOnError(err, "cannot add application")
	}

	if workspace != nil {
		// Only add to workspace if application is known to workspace
		if workspace.HasProject(project.Name) {
			action, err = actionsapplication.NewActionAddApplicationToWorkspace(ctx, &actionsapplication.AddApplicationToWorkspace{
				Name:      name,
				Project:   project.Name,
				Workspace: workspace.Name,
			})
			cli.ExitOnError(err, "cannot create action")
			_, err = actions.Run(ctx, action)
			cli.ExitOnError(err, "cannot add application to workspace")
		}

	}
	cli.Header(2, "Application <%s> added and is now active", app.Name)
}

func init() {
	ApplicationCmd.PersistentFlags().BoolVarP(&interactive, "interactive", "i", false, "interactive mode")
}
