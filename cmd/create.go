package cmd

import (
	"github.com/codefly-dev/cli/cmd/create"
	"github.com/spf13/cobra"
)

// CreateCmd represents the add command
var CreateCmd = &cobra.Command{
	Use:   "create",
	Short: "create",
}

func init() {
	CreateCmd.AddCommand(create.ProjectCmd)
}
