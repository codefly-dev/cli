package run

import (
	"context"
	"errors"
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/services/manager"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// GoCmd represents the run command
var GoCmd = &cobra.Command{
	Use:   "go",
	Short: "Run a go",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
		defer stop()

		cli.RegisterCleanup(services.ClearAgents)

		errs := make(chan error, 1) // Buffered channel

		flow, err := initRunGo(ctx, runArgs...)
		if err != nil {
			err = errors.Unwrap(err)
			cli.ExitOnError(err, "Cannot init flow")
		}
		go func() {
			errs <- runGo(ctx, flow)
		}()

	loop:
		for {
			select {
			case err := <-errs:
				cli.Error("Got go run error: %v\n", errors.Unwrap(err))
				errs <- nil
				break loop
			case <-ctx.Done():
				cli.Header(2, "Got context.Cancel: Exiting...")
				break loop
			}
		}
		stopped := <-errs
		if stopped != nil {
			cli.Error("Got error while stopping go: %v", errors.Unwrap(stopped))
		}
		err = stopGo(ctx, flow)
		cli.ExitOnError(err, "Cannot stop flow")
		cli.Header(1, "Work done!")
		cli.Exit()
	},
}

func initRunGo(ctx context.Context, args ...string) (*manager.Flow, error) {
	w := wool.Get(ctx).In("runGo")
	// Catch panic
	defer w.Catch()

	flow, err := manager.NewEmptyFlow(ctx, manager.RunMode)
	if err != nil {
		return nil, w.Wrap(err)
	}
	flow.WithStandAlone(standAlone)

	err = flow.WithGoService(ctx, args...)
	if err != nil {
		return nil, w.Wrap(err)
	}
	err = flow.CreateManager(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot create manager")
	}

	err = flow.Load(ctx)
	if err != nil {
		return nil, w.Wrap(err)
	}
	return flow, nil
}

func runGo(ctx context.Context, flow *manager.Flow) error {
	// Catch panic
	w := wool.Get(ctx).In("runGo")
	defer w.Catch()
	err := flow.Start(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot start go")
	}
	return nil
}

func stopGo(ctx context.Context, flow *manager.Flow) error {
	// Catch panic
	w := wool.Get(ctx).In("stopGo")
	defer w.Catch()
	if flow == nil {
		return nil
	}
	w.Info("Stopping services")
	err := flow.Stop()
	if err != nil {
		return w.Wrapf(err, "cannot stop go")
	}
	return nil
}

var runArgs []string

func init() {
	// Run arguments flags
	GoCmd.Flags().StringArrayVar(&runArgs, "args", []string{}, "Running arguments")

}
