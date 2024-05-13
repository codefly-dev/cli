package list

import (
	"github.com/spf13/cobra"
)

// LibraryCmd represents the run command
var LibraryCmd = &cobra.Command{
	Use:   "libraries",
	Short: "List all libraries for the workspace",

	Run: func(cmd *cobra.Command, args []string) {
		// libs := resources.MustCurrentWorkspace().Libraries
		// fmt.Println("Names:", libs)
	},
}
