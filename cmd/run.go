package cmd

import (
	"github.com/codefly-dev/cli/cmd/run"
	"github.com/spf13/cobra"
)

// RunCmd represents the run command
var RunCmd = &cobra.Command{
	Use:   "run",
	Short: "Local run of your applications",
}

func init() {
	RunCmd.AddCommand(run.ApplicationCmd)
	RunCmd.AddCommand(run.PartialCmd)
}
