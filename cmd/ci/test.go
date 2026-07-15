package ci

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// TestCmd represents the run command
var TestCmd = &cobra.Command{
	Use:   "test",
	Short: "Run CI Testing",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := common.SignalContext(ctx)
		defer stop()

		cli.Init()
		defer services.ClearAgents()

		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return err
		}

		if err := common.WithSilenceE(ctx, workspace, silent); err != nil {
			return fmt.Errorf("cannot configure silent services: %w", err)
		}

		if err := CI(ctx, workspace, runTestService); err != nil {
			return fmt.Errorf("cannot run CI tests: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		cli.Header(1, "Work done!")
		return nil
	},
}

func runTestService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) error {
	w := wool.Get(ctx).In("deployService")
	flow, err := initTestService(ctx, workspace, module, service)
	if err != nil {
		return w.Wrapf(err, "Cannot init flow")
	}
	return runAndStopFlow(flow, func() error {
		if err := testService(ctx, flow); err != nil {
			return w.Wrapf(err, "Cannot test service")
		}
		return nil
	})
}

func initTestService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) (*orchestration.Flow, error) {
	w := wool.Get(ctx).In("TestService", wool.ThisField(resources.WithUnique(service)))
	if err := resources.ValidateRuntimeContext(runtimeContext); err != nil {
		return nil, w.NewError("Invalid runtime context: %s", runtimeContext)
	}

	flow, err := orchestration.NewFlow(ctx, workspace, module, service, resources.LocalEnvironment(), orchestration.TestMode)
	if err != nil {
		return nil, w.Wrap(err)
	}
	flow.WithLoadOnly(loadOnly)
	flow.WithInitOnly(initOnly)
	flow.WithStandAlone(true)
	flow.WithRuntimeContext(runtimeContext)

	err = flow.InitManagers(ctx)
	if err != nil {
		return nil, stopFlowAfterError(flow, w.Wrap(err))
	}
	err = flow.Load(ctx)
	if err != nil {
		return nil, stopFlowAfterError(flow, w.Wrap(err))
	}
	return flow, nil
}

func testService(ctx context.Context, flow *orchestration.Flow) error {
	w := wool.Get(ctx).In("TestService")
	err := flow.Start(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot start service")
	}
	return nil
}

func init() {
	TestCmd.Flags().StringSliceVar(&silent, "silent", []string{}, "Silent services")
	TestCmd.Flags().StringVar(&runtimeContext, "runtime-context", "free", "Runtime context for the flow")
	TestCmd.Flags().StringVar(&scope, "scope", "", "Runtime scope (for testing encapsulation)")
	TestCmd.Flags().BoolVar(&initOnly, "init-only", false, "Initialize service only, i.e. without running it")
	TestCmd.Flags().BoolVar(&loadOnly, "load-only", false, "LoadRequired service only, i.e. without running it")
}
