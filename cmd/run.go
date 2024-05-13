package cmd

import (
	"github.com/codefly-dev/cli/cmd/run"
	"github.com/spf13/cobra"
)

// RunCmd represents the run command
var RunCmd = &cobra.Command{
	Use:   "run",
	Short: "Local run of your modules",
}

func init() {
	RunCmd.AddCommand(run.ServiceCmd)

	// Go
	RunCmd.AddCommand(run.GoCmd)
}
