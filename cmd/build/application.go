package build

import (
	"github.com/codefly-dev/core/agents"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/application"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
	"github.com/spf13/cobra"
)

// ApplicationCmd represents the build command
var ApplicationCmd = &cobra.Command{
	Use:   "application",
	Short: "Build an application",
	Run: func(cmd *cobra.Command, args []string) {
		ctx := shared.NewContext()
		config := common.Application(ctx)
		project := common.Project(ctx)

		golor.Println(`#(blue,bold)[Building applications]: #(italic,white)[{{ .Name }}]`, config)
		configurations.SetMode(configurations.ModeApplication)

		app, err := application.Load(ctx, project, config, application.FactoryMode)
		shared.UnexpectedExitOnError(err, "<%s>", config.Name)

		err = app.FactoryInit(ctx)
		shared.UnexpectedExitOnError(err, "<%s>", config.Name)

		err = app.Build(ctx)
		shared.UnexpectedExitOnError(err, "<%s>", config.Name)

		agents.ClearAgents()
	},
}

var current bool

func init() {
	ApplicationCmd.Flags().BoolVarP(&current, "current", "c", false, "Use the current application")
}
