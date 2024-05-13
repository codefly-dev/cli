package list

import (
	"github.com/spf13/cobra"
)

// ModuleCmd represents the run command
var ModuleCmd = &cobra.Command{
	Use:   "modules",
	Short: "List all modules for the workspace",

	Run: func(cmd *cobra.Command, args []string) {
		//mods, err := resources.ListModules()
		//shared.ExitOnError(err, "cannot list modules")
		//fmt.Println("Modules:", apps)
	},
}
