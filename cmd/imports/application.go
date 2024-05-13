package imports

import (
	"github.com/spf13/cobra"
)

// ModuleCmd represents the run command
var ModuleCmd = &cobra.Command{
	Use:   "module",
	Short: "Import an modules",

	Run: func(cmd *cobra.Command, args []string) {
		//		logger := shared.GetBaseLogger(ctx).With("import.ModuleCmd.Begin")
		//		defer logger.Catch()
		//		golor.Println(`
		//#(bold,cyan)[Very excited you decided to import an modules in codefly to 🚀!]`)
		//		path := resources.SolveDir(path)
		//		importer, err := cli.NewModuleImporter(path)
		//		cli.ExitOnError(err, "cannot create modules importer")
		//		baseImporter := &cli.ServicePrompt{}
		//		err = imports.ImportModule(&imports.Importer{
		//			ModuleImporter: importer,
		//			ServiceImporter:     baseImporter,
		//		})
		//		shared.ExitOnError(err, "cannot import modules")
	},
}

var path string

func init() {
	ModuleCmd.Flags().StringVarP(&path, "path", "p", "", "Path to the modules to import")
}
