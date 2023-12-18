package cmd

import (
	"github.com/spf13/cobra"
)

// DeployCmd represents the Deploy command
var DeployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Deploy",
}

func init() {
	// DeployCmd.AddCommand(deploy.ApplicationCmd)
	// DeployCmd.AddCommand(deploy.PartialCmd)
}
