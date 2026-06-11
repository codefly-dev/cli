package delete

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/spf13/cobra"
)

// ModuleCmd represents the run command
var ModuleCmd = &cobra.Command{
	Use:   "module",
	Short: "Delete an module",

	Run: func(cmd *cobra.Command, args []string) {
		if len(args) != 1 {
			cli.Error("You must provide a name for the module as the single argument")
			cli.ExitError()
		}
		name := args[0]
		deleteModule(name)
	},
}

func deleteModule(name string) {
	ctx, done := common.NewContext()
	defer done()

	workspace := common.Workspace(ctx)

	if !workspace.ExistsModule(name) {
		cli.Error("Module <%s> does not exist in workspace <%s>", name, workspace)
		return
	}
	confirm := models.Confirm(ctx, fmt.Sprintf("Confirm deletion of module <%s> in workspace <%s>?", name, workspace.Name), false)
	if confirm {
		err := workspace.DeleteModule(ctx, name)
		cli.ExitOnError(err, "cannot delete module")
		cli.Header(2, "Module <%s> deleted!", name)
	} else {
		cli.Header(2, "Abort! Heard loud and clear.")
	}
}
