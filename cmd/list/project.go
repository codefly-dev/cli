package list

import (
	"github.com/spf13/cobra"
)

// WorkspaceCmd represents the run command
var WorkspaceCmd = &cobra.Command{
	Use:   "workspaces",
	Short: "List all workspaces for the workspace",

	Run: func(cmd *cobra.Command, args []string) {
		//workspaces, err := resources.ListWorkspaces()
		//shared.ExitOnError(err, "cannot list workspaces")
		//fmt.Println("Workspaces:", workspaces)
	},
}
