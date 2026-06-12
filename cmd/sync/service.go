package sync

import (
	"context"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the sync command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Sync a service",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := common.SignalContext(ctx)
		defer stop()

		cli.RegisterCleanup(services.ClearAgents)

		errs := make(chan error, 1) // Buffered channel

		workspace, module, service := common.LoadRequired(ctx, args)

		flow, err := initSyncService(ctx, workspace, module, service, standAlone)
		cli.ExitOnError(err, "Cannot initialize service")
		go func() {
			errs <- common.WithHeartbeat(ctx, "syncing "+service.Name, func() error {
				return syncService(ctx, flow)
			})
		}()

		// syncErr captures a non-nil sync failure so it can be reported AFTER
		// cleanup runs. Exiting here (e.g. cli.ExitOnError) would skip
		// cleanSyncService and orphan agents/containers holding ports.
		var syncErr error
	loop:
		for {
			select {
			case err := <-errs:
				syncErr = err
				errs <- nil
				break loop
			case <-ctx.Done():
				cli.Header(2, "Got context.Cancel: Exiting...")
				break loop
			}
		}
		stopped := <-errs
		err = cleanSyncService(flow)
		if syncErr != nil {
			cli.ErrorChain(syncErr, "Got service sync error")
			cli.ExitError()
		}
		cli.ExitOnError(err, "Cannot stop flow")
		if stopped != nil {
			cli.ErrorChain(stopped, "Got error while stopping service")
			cli.ExitError()
		}
		cli.Header(1, "Work done!")
	},
}

func initSyncService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, standAlone bool) (*orchestration.Flow, error) {
	w := wool.Get(ctx).In("syncService", wool.ThisField(resources.WithUnique(service)))
	flow, err := orchestration.NewFlow(ctx, workspace, module, service, resources.LocalEnvironment(), orchestration.SyncMode)
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

func cleanSyncService(flow *orchestration.Flow) error {
	defer services.ClearAgents()
	return flow.Stop()
}

func syncService(ctx context.Context, flow *orchestration.Flow) error {
	w := wool.Get(ctx).In("syncService")
	err := flow.Sync(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot start service")
	}
	return nil

}

var standAlone bool

func init() {
	ServiceCmd.Flags().BoolVar(&standAlone, "stand-alone", false, "Begin service as standalone, i.e. without its dependencies")
	ServiceCmd.Flags().BoolVar(&initOnly, "init-only", false, "Initialize service only, i.e. without syncning it")
}
