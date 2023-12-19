package add

import (
	"github.com/spf13/cobra"
)

// PartialCmd represents the run command
var PartialCmd = &cobra.Command{
	Use:   "partial",
	Short: "Add a partial",

	Run: func(cmd *cobra.Command, args []string) {
		//project, err := configurations.CurrentProject()
		//cli.ExitOnError(err, "cannot load project")
		//partial := configurations.Partial{Name: args[0], Applications: args[1:]}
		//err = project.AddPartial(partial)
		//cli.ExitOnError(err, "cannot add partial")
		//golor.Println(`#(blue,bold)[Partial {{.Name}} added]`, partial)
	},
}

func init() {
}
