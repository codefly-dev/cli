package list

import (
	"fmt"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// ProjectCmd represents the run command
var ProjectCmd = &cobra.Command{
	Use:   "projects",
	Short: "List all projects for the project",

	Run: func(cmd *cobra.Command, args []string) {
		projects, err := configurations.ListProjects()
		shared.ExitOnError(err, "cannot list projects")
		fmt.Println("Projects:", projects)
	},
}
