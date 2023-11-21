package build

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/application"
	"github.com/codefly-dev/cli/pkg/partial"
	"github.com/codefly-dev/cli/pkg/plugins"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// PartialCmd represents the build command
var PartialCmd = &cobra.Command{
	Use:   "partial",
	Short: "Build an partial",
	Run: func(cmd *cobra.Command, args []string) {
		logger := shared.NewLogger("build.PartialCmd")
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()

		if len(args) == 0 {
			fmt.Println("Please provide a partial name")
			os.Exit(0)
		}
		name := args[0]

		project := common.ProjectConfiguration(current)

		conf, err := project.GetPartial(name)
		if err != nil {
			fmt.Printf("Cannot find partial <%s> in project <%s>\n", name, project.Name)
			os.Exit(1)
		}
		part, err := partial.NewPartial(project, conf, application.FactoryMode)
		shared.UnexpectedExitOnError(err, "<%s>", conf.Name)

		err = part.Build(ctx)
		shared.UnexpectedExitOnError(err, "cannot build partial")

		logger.Debugf("Clearing plugins")
		plugins.ClearPlugins()
		fmt.Println("Cleaning up...")
	},
}

func init() {
	PartialCmd.Flags().BoolVar(&current, "current", false, "Build the current application")
}
