package cmd

import (
	"github.com/codefly-dev/cli/cmd/initialize"
	"github.com/spf13/cobra"
)

// InitCmd represents the add command
var InitCmd = &cobra.Command{
	Use:   "init",
	Short: "init",
}

func init() {
	InitCmd.AddCommand(initialize.WorkspaceCmd)
}
