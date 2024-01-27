package deploy

import (
	"context"
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/web"
	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/configurations"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Deploy service and dependencies",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
		defer stop()

		defer agents.ClearAgents()

		errs := make(chan error, 1) // Buffered channel

		workspace := common.Workspace(ctx)
		if workspace == nil {
			cli.Error("No workspace found: can't run server")
		} else {
			go func() {
				w, err := web.NewServer(web.ServerData{Workspace: workspace})
				cli.ExitOnError(err, "cannot create web server")
				errs <- w.Start(ctx)
			}()
		}

		go func() {
			service := common.Service(ctx)
			project := common.Project(ctx)
			errs <- deployService(ctx, project, service)
		}()

	loop:
		for {
			select {
			case err := <-errs:
				cli.ExitOnError(err, "Got service run error: %v\n", err)
				// TODO: get rid when flow works
				errs <- nil
				break loop
			case <-ctx.Done():
				cli.Header(2, "Got context.Cancel: Exiting...")
				break loop
			}
		}
		stopped := <-errs
		if stopped != nil {
			cli.Error("Got error while stopping service: %v", stopped)
			return
		}
		cli.Header(1, "Service stopped successfully")
	},
}

func deployService(ctx context.Context, project *configurations.Project, service *configurations.Service) error {
	//w := wool.Get(ctx).In("runService", wool.ThisField(service))
	//flow, err := builder2.NewFlow(ctx, project, service, standAlone)
	//if err != nil {
	//	return w.Wrap(err)
	//}
	//flow.WithDeploymentEnvironment(&configurations.Environment{Name: env})
	//err = flow.Start(ctx, builder2.Deploy)
	//if err != nil {
	//	return w.Wrap(err)
	//}
	return nil
}

var standAlone bool
var env string

func init() {
	ServiceCmd.Flags().BoolVar(&standAlone, "stand-alone", false, "Deploy service as standalone, i.e. without its dependencies")
	ServiceCmd.Flags().StringVar(&env, "env", "local", "Environment to deploy to")
}
