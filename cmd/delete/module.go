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
	Short: "Remove a module and its reference from the workspace",
	Args:  cobra.ExactArgs(1),

	RunE: func(cmd *cobra.Command, args []string) error {
		return deleteModule(args[0])
	},
}

func deleteModule(name string) error {
	ctx, done := common.NewContext()
	defer done()

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}

	if !workspace.ExistsModule(name) {
		return fmt.Errorf("module <%s> does not exist in workspace <%s>", name, workspace.Name)
	}
	confirm := models.Confirm(ctx, fmt.Sprintf("Confirm deletion of module <%s> in workspace <%s>?", name, workspace.Name), false)
	if confirm {
		err := workspace.DeleteModule(ctx, name)
		if err != nil {
			return fmt.Errorf("cannot delete module: %w", err)
		}
		cli.Header(2, "Module <%s> deleted!", name)
	} else {
		cli.Header(2, "Abort! Heard loud and clear.")
	}
	return nil
}
