package cmd

import (
	"github.com/codefly-dev/cli/cmd/update"
	"github.com/spf13/cobra"
)

// UpdateCmd represents the update command
var UpdateCmd = &cobra.Command{
	Use:   "update",
	Short: "Refresh service agents or repository dependencies",
}

func init() {
	UpdateCmd.AddCommand(update.WorkspaceCmd)
	UpdateCmd.AddCommand(update.DepsCmd)
	// UpdateCmd.AddCommand(update.ModuleCmd)
	// UpdateCmd.AddCommand(update.ServiceCmd)
}
