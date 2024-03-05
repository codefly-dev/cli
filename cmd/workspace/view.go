package workspace

import (
	view_ "github.com/codefly-dev/cli/cmd/workspace/view"
	"github.com/spf13/cobra"
)

// ViewCmd represents the run command
var ViewCmd = &cobra.Command{
	Use:   "view",
	Short: "View active context",
	Run: func(cmd *cobra.Command, args []string) {
		view_.ViewApplications(view_.ViewServices)
	},
}

func init() {
	ViewCmd.AddCommand(view_.ApplicationsCmd)
	ViewCmd.AddCommand(view_.ServicesCmd)
}
