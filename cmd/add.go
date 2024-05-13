package cmd

import (
	"github.com/codefly-dev/cli/cmd/add"
	"github.com/spf13/cobra"
)

// AddCmd represents the add command
var AddCmd = &cobra.Command{
	Use:   "add",
	Short: "Add",
}

func init() {
	AddCmd.AddCommand(add.ModuleCmd)
	AddCmd.AddCommand(add.ServiceCmd)
	AddCmd.AddCommand(add.ServiceDependencyCmd)
}
