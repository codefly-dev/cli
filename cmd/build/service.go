package build

import (
	"context"
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/services/factory"
	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/runners"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the build command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Build an service",
	Run: func(cmd *cobra.Command, args []string) {

		ctx, done := common.NewContext()
		defer done()

		// Check Docker is running
		if !runners.DockerRunning(ctx) {
			cli.ExitWithMessage("Docker is not running: please start Docker")
		}

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
		defer stop()

		defer agents.ClearAgents()

		service := common.Service(ctx)
		project := common.Project(ctx)
		err := buildService(ctx, project, service)
		cli.ExitOnError(err, "Got service build error: %v\n", err)

	},
}

func buildService(ctx context.Context, project *configurations.Project, service *configurations.Service) error {
	w := wool.Get(ctx).In("cmd.build.service")
	flow, err := factory.NewFlow(ctx, project, service)
	if err != nil {
		return w.Wrapf(err, "cannot create flow")
	}
	err = flow.Start(ctx, factory.Build)
	if err != nil {
		return w.Wrapf(err, "cannot build flow")
	}
	return nil
}
