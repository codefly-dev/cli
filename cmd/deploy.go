package cmd

import (
	"github.com/codefly-dev/cli/cmd/deploy"
	"github.com/spf13/cobra"
)

// DeployCmd represents the Handle command
var DeployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Handle",
}

func init() {
	DeployCmd.AddCommand(deploy.InitCmd)
	DeployCmd.AddCommand(deploy.ServiceCmd)
	DeployCmd.AddCommand(deploy.ModuleCmd)
}
