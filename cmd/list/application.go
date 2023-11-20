package list

import (
	"fmt"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// ApplicationCmd represents the run command
var ApplicationCmd = &cobra.Command{
	Use:   "applications",
	Short: "List all applications for the project",

	Run: func(cmd *cobra.Command, args []string) {
		apps, err := configurations.ListApplications()
		shared.ExitOnError(err, "cannot list applications")
		fmt.Println("Applications:", apps)
	},
}
