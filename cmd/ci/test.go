package ci

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// TestCmd represents the run command
var testSelection SelectionFlags

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

		plan, err := testSelection.BuildPlan(ctx, workspace)
		if err != nil {
			return fmt.Errorf("cannot build affected-service plan: %w", err)
		}
		return runWithCIReport(ctx, workspace, plan, "codefly ci test", func(reporter *CIReporter) error {
			for _, suite := range normalizeTestSuites(testSuites) {
				if suite != "" {
					cli.Header(2, "CI test suite: %s", suite)
				}
				options := commandScheduleOptions(true, "test", suite, reporter)
				if err := CIWithPlanOptions(ctx, workspace, plan, runTestServiceForSuite(suite), options); err != nil {
					return fmt.Errorf("cannot run CI tests: %w", err)
				}
			}
			return ctx.Err()
		})
	},
}

func runTestService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) error {
	return runTestServiceForSuite("")(ctx, workspace, module, service)
}

func runTestServiceForSuite(suite string) Action {
	return func(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) error {
		w := wool.Get(ctx).In("deployService")
		flow, err := initTestService(ctx, workspace, module, service, suite)
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
}

func initTestService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, suite string) (*orchestration.Flow, error) {
	w := wool.Get(ctx).In("TestService", wool.ThisField(resources.WithUnique(service)))
	if err := resources.ValidateRuntimeContext(runtimeContext); err != nil {
		return nil, w.NewError("Invalid runtime context: %s", runtimeContext)
	}

	env, err := orchestration.SelectEnvironment(workspace, orchestration.LocalEnvironmentName)
	if err != nil {
		return nil, w.Wrap(err)
	}

	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.TestMode)
	if err != nil {
		return nil, w.Wrap(err)
	}
	flow.WithLoadOnly(loadOnly)
	flow.WithInitOnly(initOnly)
	flow.WithRuntimeContext(runtimeContext)
	flow.WithTestRequest(&runtimev0.TestRequest{Suite: suite})

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
	testSelection.Bind(TestCmd)
	TestCmd.Flags().StringSliceVar(&silent, "silent", []string{}, "Silent services")
	TestCmd.Flags().StringVar(&runtimeContext, "runtime-context", "free", "Runtime context for the flow")
	TestCmd.Flags().StringVar(&scope, "scope", "", "Runtime scope (for testing encapsulation)")
	TestCmd.Flags().BoolVar(&initOnly, "init-only", false, "Initialize service only, i.e. without running it")
	TestCmd.Flags().BoolVar(&loadOnly, "load-only", false, "LoadRequired service only, i.e. without running it")
	TestCmd.Flags().StringSliceVar(&testSuites, "suite", nil, "Named test suite to run (repeatable; default: each agent's advertised default)")
	bindSchedulingFlags(TestCmd)
	bindReportFlags(TestCmd)
}
