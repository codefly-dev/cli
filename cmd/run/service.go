package run

import (
	"context"
	"errors"
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
				server, err := web.NewServer(web.ServerData{Workspace: workspace})
				cli.ExitOnError(err, "cannot create web server")
				go func() {
					errs <- server.Start(ctx)
				}()
			}

		}

		service := common.Service(ctx)
		project := common.Project(ctx)
		flow, err := initRunService(ctx, project, service, standAlone, ci)
		if err != nil {
			err = errors.Unwrap(err)
			clearErr := cleanRunService(flow)
			if clearErr != nil {
				cli.Warning("Got error while cleaning up: %v", errors.Unwrap(clearErr))
			}
			cli.ExitOnError(err, "Cannot init flow")
		}
		go func() {
			errs <- runService(ctx, flow)
		}()

	loop:
		for {
			select {
			case err := <-errs:
				cli.Error("Got service run error: %v\n", errors.Unwrap(err))
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
			cli.Error("Got error while stopping service: %v", errors.Unwrap(stopped))
			return
		}
		cli.Header(1, "Work done!")
	},
}

func initRunService(ctx context.Context, project *configurations.Project, service *configurations.Service, standAlone bool, ci bool) (*manager.Flow, error) {
	w := wool.Get(ctx).In("runService", wool.ThisField(service))
	flow, err := manager.NewFlow(ctx, project, service, configurations.Local(), manager.RunMode, ci)
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
	err := flow.Stop()
	services.ClearAgents()
	return err
}

func runService(ctx context.Context, flow *manager.Flow) error {
	// Catch panic
	w := wool.Get(ctx).In("runService")
	defer w.Catch()
	err := flow.Start(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot start service")
	}
	return nil

}

var standAlone bool
var ci bool

func init() {
	ServiceCmd.Flags().BoolVar(&withServer, "server", false, "Begin service server")
	ServiceCmd.Flags().BoolVar(&ci, "ci", false, "CI Mode")
	ServiceCmd.Flags().BoolVar(&standAlone, "stand-alone", false, "Begin service as standalone, i.e. without its dependencies")
	ServiceCmd.Flags().BoolVar(&initOnly, "init-only", false, "Initialize service only, i.e. without running it")
}
