package build

import (
	"context"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/application"
	"github.com/codefly-dev/cli/pkg/plugins"
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
		ctx := context.Background()
		config := common.ApplicationConfiguration(current)
		project := common.ProjectConfiguration(current)

		golor.Println(`#(blue,bold)[Building applications]: #(italic,white)[{{ .Name }}]`, config)
		configurations.SetMode(configurations.ModeApplication)

		app, err := application.Load(project, config, application.FactoryMode)
		shared.UnexpectedExitOnError(err, "<%s>", config.Name)

		err = app.FactoryInit()
		shared.UnexpectedExitOnError(err, "<%s>", config.Name)

		err = app.Build(ctx)
		shared.UnexpectedExitOnError(err, "<%s>", config.Name)

		plugins.ClearPlugins()
	},
}

var current bool

func init() {
	ApplicationCmd.Flags().BoolVarP(&current, "current", "c", false, "Use the current application")
}
