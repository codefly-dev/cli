package expose

import (
	"fmt"

	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:    "service",
	Short:  "Expose a service",
	Hidden: true, // not implemented yet
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Not implemented — do NOT print "Work done!" (it lied about success
		// and exited 0, so scripts couldn't tell nothing happened).
		return fmt.Errorf("codefly expose service is not implemented yet")
	},
}
