package run

import (
	"context"
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/services/manager"
	"github.com/codefly-dev/cli/pkg/services/services"
	"github.com/codefly-dev/cli/pkg/web"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Begin a service",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
		defer stop()

		errs := make(chan error, 1) // Buffered channel

		if withServer {
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

		}

		service := common.Service(ctx)
		project := common.Project(ctx)
		flow, err := initRunService(ctx, project, service, standAlone)
		cli.ExitOnError(err, "Cannot initialize service")
		go func() {
			errs <- runService(ctx, flow)
		}()

	loop:
		for {
			select {
			case err := <-errs:
				cli.ExitOnError(err, "Got service run error: %v\n", err)
				errs <- nil
				break loop
			case <-ctx.Done():
				cli.Header(2, "Got context.Cancel: Exiting...")
				break loop
			}
		}
		stopped := <-errs
		err = cleanRunService(flow)
		cli.ExitOnError(err, "Cannot stop flow")
		if stopped != nil {
			cli.Error("Got error while stopping service: %v", stopped)
			return
		}
		cli.Header(1, "Service stopped successfully")
	},
}

func initRunService(ctx context.Context, project *configurations.Project, service *configurations.Service, standAlone bool) (*manager.Flow, error) {
	w := wool.Get(ctx).In("runService", wool.ThisField(service))
	flow, err := manager.NewFlow(ctx, project, service, configurations.Local(), manager.RunMode)
	if err != nil {
		return nil, w.Wrap(err)
	}
	flow.WithStandAlone(standAlone)
	err = flow.Load(ctx)
	if err != nil {
		return nil, w.Wrap(err)
	}
	return flow, nil
}

func cleanRunService(flow *manager.Flow) error {
	services.ClearAgents()
	return flow.Stop()
}

func runService(ctx context.Context, flow *manager.Flow) error {
	w := wool.Get(ctx).In("runService")
	err := flow.Start(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot start service")
	}
	return nil

}

var standAlone bool

func init() {
	ServiceCmd.Flags().BoolVar(&withServer, "server", true, "Begin service server")
	ServiceCmd.Flags().BoolVar(&standAlone, "stand-alone", false, "Begin service as standalone, i.e. without its dependencies")
	ServiceCmd.Flags().BoolVar(&initOnly, "init-only", false, "Initialize service only, i.e. without running it")
}
