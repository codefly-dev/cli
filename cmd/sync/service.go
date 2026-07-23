package sync

import (
	"context"
	"errors"
	"fmt"

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
	Short: "Regenerate a service's dependency-derived configuration through its agent",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := common.SignalContext(ctx)
		defer stop()

		cli.RegisterCleanup(services.ClearAgents)

		workspace, module, service, err := common.LoadRequiredE(ctx, args)
		if err != nil {
			return err
		}

		flow, err := initSyncService(ctx, workspace, module, service, standAlone)
		if err != nil {
			return fmt.Errorf("cannot initialize service: %w", err)
		}
		cleaned := false
		cleanup := func() error {
			if cleaned {
				return nil
			}
			cleaned = true
			return cleanSyncService(flow)
		}
		defer func() { _ = cleanup() }()

		syncErr := common.WithHeartbeat(ctx, "syncing "+service.Name, func() error {
			return syncService(ctx, flow)
		})
		stopErr := cleanup()
		var result []error
		if syncErr != nil {
			result = append(result, fmt.Errorf("service sync failed: %w", syncErr))
		}
		if stopErr != nil {
			result = append(result, fmt.Errorf("cannot stop flow: %w", stopErr))
		}
		if len(result) > 0 {
			return errors.Join(result...)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		cli.Header(1, "Work done!")
		return nil
	},
}

func initSyncService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, standAlone bool) (*orchestration.Flow, error) {
	w := wool.Get(ctx).In("syncService", wool.ThisField(resources.WithUnique(service)))
	env, err := orchestration.SelectEnvironment(workspace, orchestration.LocalEnvironmentName)
	if err != nil {
		return nil, w.Wrap(err)
	}
	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.SyncMode)
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
