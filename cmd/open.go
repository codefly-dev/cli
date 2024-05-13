package cmd

import (
	"github.com/codefly-dev/cli/cmd/open"
	"github.com/spf13/cobra"
)

// OpenCmd represents the build command
var OpenCmd = &cobra.Command{
	Use:   "open",
	Short: "Open your workspace and module automatically",
}

func init() {
	OpenCmd.AddCommand(open.WorkspaceCmd)
	OpenCmd.AddCommand(open.ModuleCmd)
	OpenCmd.AddCommand(open.ServiceCmd)
}
