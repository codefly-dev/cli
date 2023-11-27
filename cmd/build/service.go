package build

import (
	"context"
	"os"

	"github.com/codefly-dev/cli/pkg/services"
	"github.com/codefly-dev/core/agents"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"

	"github.com/spf13/cobra"
)

// ServiceCmd represents the build command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Build an service",
	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.Background()
		logger := shared.NewLogger("build.ServiceCmd")
		if len(args) == 0 {
			logger.Oops("Please provide a service name")
			os.Exit(1)
		}

		project := common.ProjectConfiguration(current)

		name := args[0]
		reference, err := configurations.ParseServiceReference(name)
		shared.UnexpectedExitOnError(err, "<%s>", name)

		var app *configurations.Application
		if reference.Application == "" {
			app = common.ApplicationConfiguration(true)
		} else {
			app, err = configurations.LoadApplicationFromName(reference.Application, configurations.WithProject(project))
			shared.UnexpectedExitOnError(err, "<%s>", reference.Application)
		}

		conf, err := app.LoadServiceFromName(reference.Name)
		shared.UnexpectedExitOnError(err, "<%s>", name)

		instance, err := services.NewServiceInstance(conf, app)
		shared.UnexpectedExitOnError(err, "<%s>", name)

		err = instance.SoloBuild(ctx)
		shared.UnexpectedExitOnError(err, "cannot build")

		agents.ClearAgents()
	},
}
