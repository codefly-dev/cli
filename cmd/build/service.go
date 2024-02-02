package build

import (
	"context"
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/services/manager"
	"github.com/codefly-dev/cli/pkg/services/services"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the build command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Build a service",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
		defer stop()

		defer services.ClearAgents()

		errs := make(chan error, 1) // Buffered channel

		service := common.Service(ctx)
		project := common.Project(ctx)
		flow, err := initBuildService(ctx, project, service, standAlone)
		cli.ExitOnError(err, "Cannot initialize service")
		go func() {
			errs <- buildService(ctx, flow)
		}()

	loop:
		for {
			select {
			case err := <-errs:
				cli.ExitOnError(err, "Got service build error: %v\n", err)
				errs <- nil
				break loop
			case <-ctx.Done():
				cli.Header(2, "Got context.Cancel: Exiting...")
				break loop
			}
		}
		stopped := <-errs
		err = flow.Stop()
		cli.ExitOnError(err, "Cannot stop flow")
		if stopped != nil {
			cli.Error("Got error while stopping service: %v", stopped)
			return
		}
		cli.Header(1, "Service stopped successfully")
	},
}

func initBuildService(ctx context.Context, project *configurations.Project, service *configurations.Service, standAlone bool) (*manager.Flow, error) {
	w := wool.Get(ctx).In("buildService", wool.ThisField(service))
	flow, err := manager.NewFlow(ctx, project, service, configurations.Local(), manager.BuildMode)
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

func cleanBuildService(flow *manager.Flow) error {
	defer services.ClearAgents()
	return flow.Stop()
}

func buildService(ctx context.Context, flow *manager.Flow) error {
	w := wool.Get(ctx).In("buildService")
	err := flow.Build(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot start service")
	}
	return nil

}

var standAlone bool

func init() {
	ServiceCmd.Flags().BoolVar(&standAlone, "stand-alone", false, "Begin service as standalone, i.e. without its dependencies")
}
