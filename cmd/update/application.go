package update

import (
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/application"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
	"github.com/spf13/cobra"
)

// ApplicationCmd represents the run command
var ApplicationCmd = &cobra.Command{
	Use:   "application",
	Short: "Update an application",

	Run: func(cmd *cobra.Command, args []string) {
		ctx := shared.NewContext()
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
		defer stop()

		project := common.Project(ctx)
		config := common.Application(ctx)

		golor.Println(`#(blue,bold)[Starting application]: #(italic,white)[{{ .Name }}]
#(blue,bold)[Ctrl-C anytime to exit...]`, config)

		configurations.SetMode(configurations.ModeApplication)
		app, err := application.Load(ctx, project, config, application.FactoryMode)
		shared.UnexpectedExitOnError(err, "<%s>", config.Name)

		err = app.Update(ctx)
		shared.UnexpectedExitOnError(err, "<%s>", config.Name)
	},
}

var current bool

func init() {
	ApplicationCmd.Flags().BoolVarP(&current, "current", "c", false, "update current application")
	ApplicationCmd.PersistentFlags().BoolVarP(&interactive, "interactive", "i", false, "interactive mode")
}
