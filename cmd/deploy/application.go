package deploy

import (
	"context"

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
	Short: "Deploy an application",

	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.Background()
		project := common.ProjectConfiguration(current)
		config := common.ApplicationConfiguration(current)

		configurations.SetMode(configurations.ModeApplication)
		app, err := application.Load(project, config, application.FactoryMode)
		shared.UnexpectedExitOnError(err, "<%s>", config.Name)

		golor.Println(`#(blue,bold)[Deploying applications]: #(italic,white)[{{.Configuration.Name}}]`, app)

		env, err := project.FindEnvironment(environment)
		shared.UnexpectedExitOnError(err, "cannot find environment <%s>", environment)

		err = app.Deploy(ctx, env)
		shared.UnexpectedExitOnError(err, "cannot deploy applications")
	},
}

func init() {
	ApplicationCmd.Flags().BoolVar(&current, "current", false, "Deploy the current application")
	ApplicationCmd.Flags().StringVar(&environment, "environment", "", "Deploy the application in the given environment")
}
