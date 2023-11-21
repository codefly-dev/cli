package sync

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
	Short: "Sync an application",

	Run: func(cmd *cobra.Command, args []string) {
		logger := shared.NewLogger("SyncCmd.Name")

		ctx := context.Background()
		project := common.ProjectConfiguration(current)

		var config *configurations.Application
		var err error
		// Optional application argument
		if len(args) > 0 {
			name := args[0]
			config, err = configurations.LoadApplicationFromName(name, configurations.WithProject(project))
			shared.ExitOnError(err, "cannot load application <%s>", name)
		} else {
			config = common.ApplicationConfiguration(current)
		}

		configurations.SetMode(configurations.ModeApplication)
		app, err := application.Load(project, config, application.FactoryMode)

		shared.UnexpectedExitOnError(err, "<%s>", config.Name)

		others, err := project.OtherApplications(config)
		shared.UnexpectedExitOnError(err, "cannot get other applications")
		for _, other := range others {
			logger.Debugf("not loading other application -- this is what partial are for: %v", other.Name)
			// otherApp, err := application.Load(other, project)
			// shared.UnexpectedExitOnError(err, "<%s>", other.Name)
			// app.AddDependency(otherApp)
		}

		logger.Debugf("other applications: %d", len(others))
		golor.Println(`#(blue,bold)[Syncing application]: #(italic,white)[{{.Configuration.Name}}]`, app)

		err = app.Sync(ctx)
		shared.UnexpectedExitOnError(err, "cannot sync application")
	},
}

func init() {
	ApplicationCmd.Flags().BoolVar(&current, "current", false, "Run the current application")
}
