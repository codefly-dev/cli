package create

import (
	"fmt"
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/actions/actions"
	actionsworkspace "github.com/codefly-dev/core/actions/workspace"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

// WorkspaceCmd represents the run command
var WorkspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Create a new workspace",

	Run: func(cmd *cobra.Command, args []string) {

		if interactive {
			cli.Error("Interactive mode not implemented yet")
			cli.Exit()
		}
		if len(args) == 1 {
			name := args[0]
			newWorkspace(name)
		}
	},
}

func newWorkspace(name string) {
	ctx, done := common.NewContext()
	defer done()

	cur, err := os.Getwd()
	cli.ExitOnError(err, "Cannot get current directory")

	confirm := models.Confirm(ctx, fmt.Sprintf("codefly will create a workspace <%s> in the current folder. Proceed?", name), true)
	if !confirm {
		cli.Header(2, "Received loud and clear!")
	}

	// What style for the workspace
	entries := []*models.Entry{
		{Identifier: resources.LayoutKindFlat, Description: "Flat layout (no modules), good for simple projects", Current: true},
		{Identifier: resources.LayoutKindModules, Description: "Modules layout, good for complex projects with multiple components"},
	}
	choice, err := models.Choice(ctx, "Choose the style of the workspace", entries)
	cli.ExitOnError(err, "Cannot get choice")
	var action actions.Action
	action, err = actionsworkspace.NewActionNewWorkspace(ctx, &actionsworkspace.NewWorkspace{
		Name:   name,
		Layout: choice.Identifier,
		Path:   cur,
	})
	cli.ExitOnError(err, "cannot create action")

	out, err := actions.Run(ctx, action, nil)
	if err != nil {
		cli.ExitOnError(err, "cannot add workspace")
	}

	workspace, err := actions.As[resources.Workspace](out)
	if err != nil {
		cli.ExitOnError(err, "cannot add workspace")
	}

	cli.Header(2, "Workspace <%s> created in current directory", workspace.Name)

}

func init() {
	WorkspaceCmd.PersistentFlags().BoolVarP(&interactive, "interactive", "i", false, "interactive mode")
}
