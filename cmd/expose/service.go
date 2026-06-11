package expose

import (
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:    "service",
	Short:  "Expose a service",
	Hidden: true, // not implemented yet
	Run: func(cmd *cobra.Command, args []string) {
		// Not implemented — do NOT print "Work done!" (it lied about success
		// and exited 0, so scripts couldn't tell nothing happened).
		cli.Error("`codefly expose service` is not implemented yet")
		cli.ExitError()
	},
}
