package cmd

import (
	"github.com/codefly-dev/cli/cmd/deploy"
	"github.com/spf13/cobra"
)

// DeployCmd represents the Deploy command
var DeployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Deploy",
}

func init() {
	DeployCmd.AddCommand(deploy.InitCmd)
	DeployCmd.AddCommand(deploy.ServiceCmd)
	DeployCmd.AddCommand(deploy.KustomizeCmd)
}
