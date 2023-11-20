package update

import (
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Update an service",

	Run: func(cmd *cobra.Command, args []string) {
		//_, stop := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
		//defer stop()
		//
		//logger := shared.NewLogger("update.ServiceCmd")
		//if interactive {
		//	logger.Oops("interactive mode not supported yet")
		//	os.Exit(0)
		//}
		//if len(args) != 1 {
		//	logger.Oops("service name is required")
		//	os.Exit(0)
		//}
		//name := args[0]
		//
		//app := common.ApplicationConfiguration(current)
		//svc, err := configurations.FindServiceFromName(name, configurations.WithApplication(app))
		//shared.UnexpectedExitOnError(err, "cannot find service")
		//
		//err = service.Update(svc, app)
		//shared.UnexpectedExitOnError(err, "<%s>", svc.Name)
	},
}

func init() {
	ServiceCmd.Flags().BoolVarP(&current, "current", "c", false, "update current application")
}
