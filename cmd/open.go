package cmd

import (
	"github.com/codefly-dev/cli/cmd/open"
	"github.com/spf13/cobra"
)

// OpenCmd represents the build command
var OpenCmd = &cobra.Command{
	Use:   "open",
	Short: "Open a workspace, module, or service in your configured editor",
}

func init() {
	OpenCmd.AddCommand(open.WorkspaceCmd)
	OpenCmd.AddCommand(open.ModuleCmd)
	OpenCmd.AddCommand(open.ServiceCmd)
}
