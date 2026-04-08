package test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/tui"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Test a service",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
		defer stop()

		cli.Init()
		cli.RegisterCleanup(services.ClearAgents)

		workspace, module, service := common.LoadRequired(ctx, args)
		serviceName := resources.WithUnique(service).Unique()
		isHeadless := headless || !term.IsTerminal(int(os.Stdout.Fd()))

		var flow *orchestration.Flow

		if isHeadless {
			fmt.Printf("[codefly] Testing service %s (headless mode)\n", serviceName)
			var err error
			flow, err = initRunService(ctx, workspace, module, service)
			if err != nil {
				cli.Error("init failed: %v", errors.Unwrap(err))
				cli.Exit()
				return
			}
			err = testService(ctx, flow)
			if err != nil {
				cli.Error("test failed: %v", err)
			} else {
				fmt.Printf("[codefly] Tests passed for %s\n", serviceName)
			}
		} else {
			logCh := tui.NewLogChannel()
			cli.SuppressOutput()

			tuiErr := tui.RunServiceTUI(serviceName, logCh, func(t *tui.ServiceTUI) {
				t.SendState(serviceName, tui.StateLoading)

				var err error
				flow, err = initRunService(ctx, workspace, module, service)
				if err != nil {
					err = errors.Unwrap(err)
					t.SendError(err)
					return
				}

				t.SendState(serviceName, tui.StateTesting)

				err = testService(ctx, flow)
				if err != nil {
					t.SendError(err)
					return
				}

				t.SendDone(nil)
			})
			if tuiErr != nil {
				cli.Error("TUI error: %v", tuiErr)
			}
		}

		_ = stopService(ctx, flow)
		cli.Exit()
	},
}

func initRunService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) (*orchestration.Flow, error) {
	w := wool.Get(ctx).In("testService", wool.ThisField(resources.WithUnique(service)))
	defer w.Catch()

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
		return nil, w.Wrap(err)
	}
	err = flow.Load(ctx)
	if err != nil {
		return nil, w.Wrap(err)
	}
	return flow, nil
}

func testService(ctx context.Context, flow *orchestration.Flow) error {
	w := wool.Get(ctx).In("testService")
	defer w.Catch()
	err := flow.Start(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot start service")
	}
	return nil
}

func stopService(ctx context.Context, flow *orchestration.Flow) error {
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
	ServiceCmd.Flags().StringVar(&runtimeContext, "runtime-context", "free", "Runtime context for the flow")
	ServiceCmd.Flags().StringVar(&scope, "scope", "", "Runtime scope (for testing encapsulation)")
	ServiceCmd.Flags().BoolVar(&initOnly, "init-only", false, "Initialize service only, i.e. without running it")
	ServiceCmd.Flags().BoolVar(&loadOnly, "load-only", false, "LoadRequired service only, i.e. without running it")
	ServiceCmd.Flags().BoolVar(&headless, "headless", false, "Run without TUI (auto-enabled when no TTY)")
}
