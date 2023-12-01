package delete

import (
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/golor"
	"github.com/spf13/cobra"
)

// ProjectCmd represents the run command
var ProjectCmd = &cobra.Command{
	Use:   "project",
	Short: "Delete an project",

	Run: func(cmd *cobra.Command, args []string) {
		name := args[0]
		configurations.Global().DeleteProject(name)
		golor.Println(`Project deleted!`)
	},
}
