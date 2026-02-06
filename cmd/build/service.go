package build

import (
	"context"
	"errors"
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/builder"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/wool"
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

		cli.RegisterCleanup(services.ClearAgents)

		errs := make(chan error, 1) // Buffered channel

		workspace, module, service := common.LoadRequired(ctx, args)

		flow, err := initBuildService(ctx, workspace, module, service, standAlone)
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
		err = cleanBuildService(flow)
		cli.ExitOnError(err, "Cannot stop flow")
		if stopped != nil {
			cli.Error("Got error while stopping service: %v", errors.Unwrap(stopped))
			return
		}
		cli.Header(1, "Work done!")
	},
}

func initBuildService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, standAlone bool) (*orchestration.Flow, error) {
	w := wool.Get(ctx).In("buildService", wool.ThisField(resources.WithUnique(service)))
	if org != "" {
		repo := "621829027644.dkr.ecr.us-east-1.amazonaws.com/kopkfeqwuk"
		builder.SetRepository(repo)
	}
	if push {
		orchestration.SetBuilderPush()
	}
	flow, err := orchestration.NewFlow(ctx, workspace, module, service, resources.LocalEnvironment(), orchestration.BuildMode)
	if err != nil {
		return nil, w.Wrap(err)
	}
	flow.WithStandAlone(standAlone)
	err = flow.InitManagers(ctx)
	if err != nil {
		return nil, w.Wrap(err)
	}
	err = flow.Load(ctx)
	if err != nil {
		return nil, w.Wrap(err)
	}
	return flow, nil
}

func cleanBuildService(flow *orchestration.Flow) error {
	defer services.ClearAgents()
	return flow.Stop()
}

func buildService(ctx context.Context, flow *orchestration.Flow) error {
	w := wool.Get(ctx).In("buildService")
	err := flow.Build(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot start service")
	}
	return nil

}

var standAlone bool
var org string
var push bool

func init() {
	ServiceCmd.Flags().BoolVar(&standAlone, "stand-alone", false, "Begin service as standalone, i.e. without its dependencies")
	ServiceCmd.Flags().StringVar(&org, "org", "", "Organization")
	ServiceCmd.Flags().BoolVar(&push, "push", false, "Push the image to the repository")
}
