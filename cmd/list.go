package cmd

import (
	"github.com/codefly-dev/cli/cmd/list"
	"github.com/spf13/cobra"
)

// ListCmd represents the list command
var ListCmd = &cobra.Command{
	Use:   "list",
	Short: "List jobs and other resources in the current workspace",
}

func init() {
	ListCmd.AddCommand(list.WorkspaceCmd)
	ListCmd.AddCommand(list.ModuleCmd)
	ListCmd.AddCommand(list.LibraryCmd)
	ListCmd.AddCommand(list.JobsCmd)
}
