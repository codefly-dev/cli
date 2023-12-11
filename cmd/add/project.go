package add

import (
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/prompts"
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
		logger := shared.GetLogger(ctx).With("Add project")
		if interactive {
			logger.Oops("Interactive mode not implemented yet")
		}
		if len(args) != 1 {
			logger.Oops("You must provide a name for the project as the single argument")
		}
		addProject(args[0])
	},
}

func addProject(name string) {
	ctx := shared.NewContext()
	workspace := common.Workspace(ctx)

	if workspace.ExistsProject(name) {
		cli.Error("Project <{{.}}> already exists", name)
		os.Exit(0)
	}

	// Asks for Description
	addDescription := prompts.Confirm("Do you want to add a short description?", false)
	var desc string
	if addDescription {
		desc = prompts.Input("Description", "Make some magic 🪄")
	}
	action, err := actionsproject.NewActionAddProject(ctx, &actionsproject.AddProject{
		InWorkspace: workspace.Name,
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
