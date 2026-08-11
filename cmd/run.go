package cmd

import (
	"github.com/codefly-dev/cli/cmd/run"
	"github.com/spf13/cobra"
)

// RunCmd represents the run command
var RunCmd = &cobra.Command{
	Use:   "run",
	Short: "Start a service or job in its local workspace context",
}

func init() {
	RunCmd.AddCommand(run.ServiceCmd)
	RunCmd.AddCommand(run.JobCmd)
	RunCmd.AddCommand(run.CommandCmd)
}
