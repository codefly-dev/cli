package context

import (
	view_ "github.com/codefly-dev/cli/cmd/context/view"
	"github.com/spf13/cobra"
)

// ViewCmd represents the run command
var ViewCmd = &cobra.Command{
	Use:   "view",
	Short: "View active context",
}

func init() {
	ViewCmd.AddCommand(view_.ApplicationsCmd)
	ViewCmd.AddCommand(view_.ServicesCmd)
}
