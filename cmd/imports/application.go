package imports

import (
	"github.com/spf13/cobra"
)

// ApplicationCmd represents the run command
var ApplicationCmd = &cobra.Command{
	Use:   "application",
	Short: "Import an applications",

	Run: func(cmd *cobra.Command, args []string) {
		//		logger := shared.GetBaseLogger(ctx).With("import.ApplicationCmd.Start")
		//		defer logger.Catch()
		//		golor.Println(`
		//#(bold,cyan)[Very excited you decided to import an applications in codefly to 🚀!]`)
		//		path := configurations.SolveDir(path)
		//		importer, err := cli.NewApplicationImporter(path)
		//		cli.ExitOnError(err, "cannot create applications importer")
		//		baseImporter := &cli.ServicePrompt{}
		//		err = imports.ImportApplication(&imports.Importer{
		//			ApplicationImporter: importer,
		//			ServiceImporter:     baseImporter,
		//		})
		//		shared.ExitOnError(err, "cannot import applications")
	},
}

var path string

func init() {
	ApplicationCmd.Flags().StringVarP(&path, "path", "p", "", "Path to the applications to import")
}
