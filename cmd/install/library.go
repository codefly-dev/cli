package install

import (
	"github.com/spf13/cobra"
)

// LibraryCmd represents the run command
var LibraryCmd = &cobra.Command{
	Use:   "library",
	Short: "Install a library",

	Run: func(cmd *cobra.Command, args []string) {
		//logger := shared.GetBaseLogger(ctx).With("install.LibraryCmd")
		//ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
		//
		//defer stop()
		//go func() {
		//	<-ctx.Done()
		//	fmt.Println("Exiting...")
		//	os.Exit(0)
		//}()
		//
		//name := args[0]
		//
		//// Setup probably should be moved other places...getting busy
		//destination = configurations.SolveDir(destination)
		//// We infer the path of the library from the destination
		//logger.TODO("Make a function on project to get relative path")
		//relativePath, err := filepath.Rel(configurations.MustCurrentProject().Dir(), destination)
		//shared.ExitOnError(err, "cannot get relative path")
		//
		//err = libraries.Setup(destination, override)
		//shared.ExitOnError(err, "cannot setup library")
		//
		//logger.Message("Installing library <%s> to <%s>", name, destination)
		//
		//lib, err := configurations.ParseLibrary(name)
		//shared.ExitOnError(err, "cannot parse library name: %s", name)
		//lib.RelativePathOverride = relativePath
		//
		//err = libraries.Install(lib)
		//shared.ExitOnError(err, "cannot install library")
		//
		//err = configurations.AddLibraryUsage(lib, destination)
		//shared.ExitOnError(err, "cannot add library usage")
	},
}

var (
	destination string
	override    bool
)

func init() {
	LibraryCmd.PersistentFlags().StringVar(&destination, "destination", "", "Address of the library")
	LibraryCmd.PersistentFlags().BoolVar(&override, "override", false, "Replace destination directory")
}
