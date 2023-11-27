package add

import (
	"context"
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
	"github.com/spf13/cobra"
)

// ApplicationCmd represents the run command
var ApplicationCmd = &cobra.Command{
	Use:   "application",
	Short: "Add an application",

	Run: func(cmd *cobra.Command, args []string) {
		_, stop := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
		defer stop()

		if len(args) == 0 || interactive {
			shared.Exit(`🥸Provide a name for your application as the argument.
Interactive mode to be supported soon.`)
		}
		project := common.ProjectConfiguration(current)
		if project == nil {
			shared.Exit("cannot find project")
		}

		name := args[0]
		app, err := configurations.NewApplication(name)
		if err != nil {
			shared.ExitOnError(err, "cannot create application <%s>", name)
		}

		golor.Println(`#(blue)[Successfully created your application <{{.Name}}>!]
#(italic,blue)[You are ready to add some services to it!]
#(italic,white)[codefly create service <service-name> --agent=<base>]

Go to the website to look for services to get started: never start from a blank page!!

See #(italic,white)[codefly create service --help] for more information.

#(italic,white)[You can also open your applications in your favorite IDE by running]
codefly open
`, app)
		shared.UnexpectedExitOnError(project.AddApplication(app.Reference()), "cannot add application")
		shared.UnexpectedExitOnError(project.Save(), "cannot save project")
	},
}

func init() {
	ApplicationCmd.PersistentFlags().BoolVarP(&interactive, "interactive", "i", false, "interactive mode")
}
