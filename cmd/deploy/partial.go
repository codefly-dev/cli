package deploy

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/codefly-dev/core/agents"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/application"
	"github.com/codefly-dev/cli/pkg/partial"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// PartialCmd represents the build command
var PartialCmd = &cobra.Command{
	Use:   "partial",
	Short: "Deploy an partial",
	Run: func(cmd *cobra.Command, args []string) {
		ctx := shared.NewContext()
		logger := shared.GetLogger(ctx).With("deploy.PartialCmd")
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()

		if len(args) == 0 {
			fmt.Println("Please provide a partial name")
			os.Exit(0)
		}
		name := args[0]

		project := common.Project(ctx)

		conf, err := project.GetPartial(name)
		if err != nil {
			fmt.Printf("Cannot find partial <%s> in project <%s>\n", name, project.Name)
			os.Exit(1)
		}
		part, err := partial.NewPartial(ctx, project, conf, application.FactoryMode)
		shared.UnexpectedExitOnError(err, "<%s>", conf.Name)

		env, err := project.FindEnvironment(environment)
		shared.UnexpectedExitOnError(err, "cannot find environment <%s>", environment)

		err = part.Deploy(ctx, env)
		shared.UnexpectedExitOnError(err, "cannot deploy partial")

		logger.Debugf("Clearing agents")
		agents.ClearAgents()
		fmt.Println("Cleaning up...")
	},
}

func init() {
	PartialCmd.Flags().BoolVar(&current, "current", false, "Build the current application")
	PartialCmd.Flags().StringVar(&environment, "environment", "", "Deploy the application in the given environment")
}
