package list

import (
	"github.com/spf13/cobra"
)

// LibraryCmd represents the run command
var LibraryCmd = &cobra.Command{
	Use:   "libraries",
	Short: "List all libraries for the project",

	Run: func(cmd *cobra.Command, args []string) {
		// libs := configurations.MustCurrentProject().Libraries
		// fmt.Println("Names:", libs)
	},
}
