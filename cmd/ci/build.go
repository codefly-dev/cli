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

// BuildCmd represents the run command
var buildSelection SelectionFlags

var BuildCmd = &cobra.Command{
	Use:   "build",
	Short: "Build affected services as a CI stage",
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

		plan, err := buildSelection.BuildPlan(ctx, workspace)
		if err != nil {
			return fmt.Errorf("cannot build affected-service plan: %w", err)
		}
		return runWithCIReport(ctx, workspace, plan, "codefly ci build", func(reporter *CIReporter) error {
			options := commandScheduleOptions(false, "build", "", reporter)
			if err := CIWithPlanOptions(ctx, workspace, plan, runBuildService, options); err != nil {
				return fmt.Errorf("cannot run CI build: %w", err)
			}
			return ctx.Err()
		})
	},
}

func runBuildService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) error {
	w := wool.Get(ctx).In("buildService")
	flow, err := initBuildService(ctx, workspace, module, service)
	if err != nil {
		return w.Wrapf(err, "Cannot init flow")
	}
	return runAndStopFlow(flow, func() error {
		if err := buildService(ctx, flow); err != nil {
			return w.Wrapf(err, "Cannot build service")
		}
		return nil
	})
}

func initBuildService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) (*orchestration.Flow, error) {
	w := wool.Get(ctx).In("BuildService", wool.ThisField(resources.WithUnique(service)))
	if err := resources.ValidateRuntimeContext(runtimeContext); err != nil {
		return nil, w.NewError("Invalid runtime context: %s", runtimeContext)
	}

	env, err := orchestration.SelectEnvironment(workspace, orchestration.LocalEnvironmentName)
	if err != nil {
		return nil, w.Wrap(err)
	}

	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.BuildMode)
	if err != nil {
		return nil, w.Wrap(err)
	}

	flow.WithOutputSink(cli.NewOutputSink())
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

func buildService(ctx context.Context, flow *orchestration.Flow) error {
	w := wool.Get(ctx).In("BuildService")
	err := flow.Build(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot start service")
	}
	return nil
}

func init() {
	buildSelection.Bind(BuildCmd)
	BuildCmd.Flags().StringSliceVar(&silent, "silent", []string{}, "Silent mode")
	BuildCmd.Flags().StringVar(&runtimeContext, "runtime-context", "free", "Runtime context for the flow")
	BuildCmd.Flags().StringVar(&scope, "scope", "", "Runtime scope (for testing encapsulation)")
	BuildCmd.Flags().BoolVar(&initOnly, "init-only", false, "Initialize service only, i.e. without running it")
	BuildCmd.Flags().BoolVar(&loadOnly, "load-only", false, "LoadRequired service only, i.e. without running it")
	bindSchedulingFlags(BuildCmd)
	bindReportFlags(BuildCmd)
}
