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
		cli.Init()
		cli.RegisterCleanup(services.ClearAgents)

		errs := make(chan error, 1) // Buffered channel

		if withServer {
			workspace := common.Workspace(ctx)
			server, err := web.NewServer(web.ServerData{Workspace: workspace})
			cli.ExitOnError(err, "cannot create web server")
			go func() {
				errs <- server.Start(ctx)
			}()
		}
		project := common.RequireProject(ctx)

		// Argument overrides directory
		service, err := common.ParseServiceArgument(ctx, project, args)
		if err != nil {
			cli.ExitOnError(err, "Cannot parse service argument")
		}
		if service == nil {
			service = common.Service(ctx)
		}

		if service == nil {
			cli.Error("No service found: use argument or run inside a service folder or use workspace")
			return
		}
		flow, err := initRunService(ctx, project, service, standAlone, ci)
		if err != nil {
			err = errors.Unwrap(err)
			cli.ExitOnError(err, "Cannot init flow")
		}
		go func() {
			errs <- runService(ctx, flow)
		}()

	loop:
		for {
			select {
			case err := <-errs:
				if err != nil {
					cli.Error("Got service run error: %v\n", errors.Unwrap(err))
				}
				errs <- nil
				break loop
			case <-ctx.Done():
				cli.Header(2, "Got context.Cancel: Exiting...")
				break loop
			}
		}
		stopped := <-errs
		if stopped != nil {
			cli.Error("Got error while stopping service: %v", errors.Unwrap(stopped))
		}
		err = stopService(ctx, flow)
		cli.ExitOnError(err, "Cannot stop flow")
		cli.Header(1, "Work done!")
		cli.Exit()
	},
}

func initRunService(ctx context.Context, project *configurations.Project, service *configurations.Service, standAlone bool, ci bool) (*manager.Flow, error) {
	w := wool.Get(ctx).In("runService", wool.ThisField(service))
	// Catch panic
	defer w.Catch()

	flow, err := manager.NewFlow(ctx, project, service, configurations.Local(), manager.RunMode)
	if err != nil {
		return nil, w.Wrap(err)
	}
	flow.WithLoadOnly(loadOnly)
	flow.WithInitOnly(initOnly)
	flow.WithStandAlone(standAlone)
	flow.WithExcludeRoot(excludeRoot)
	flow.WithNative(native)
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

func stopService(ctx context.Context, flow *manager.Flow) error {
	// Catch panic
	w := wool.Get(ctx).In("stopService")
	defer w.Catch()
	if flow == nil {
		return nil
	}
	w.Info("Stopping services")
	err := flow.Stop()
	if err != nil {
		return w.Wrapf(err, "cannot stop service")
	}
	return nil
}

func init() {
	ServiceCmd.Flags().BoolVar(&withServer, "server", false, "Begin service server")
	ServiceCmd.Flags().BoolVar(&native, "native", false, "Native mode")
	ServiceCmd.Flags().BoolVar(&standAlone, "stand-alone", false, "Begin service as standalone, i.e. without its dependencies")
	ServiceCmd.Flags().BoolVar(&excludeRoot, "exclude-root", false, "Exclude root service")
	ServiceCmd.Flags().BoolVar(&initOnly, "init-only", false, "Initialize service only, i.e. without running it")
	ServiceCmd.Flags().BoolVar(&loadOnly, "load-only", false, "Load service only, i.e. without running it")
}
