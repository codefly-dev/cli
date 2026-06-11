package test

import (
	"context"
	"fmt"
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
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
	Long: `Test a service via the agent's Test RPC.

All flags are forwarded to the agent, which maps them to its native test runner:
  Go (go test):       --filter → -run "(p1|p2)", --race, --timeout, --coverage
  JS (vitest):        --filter → --testNamePattern, --suite=e2e → npm run test:e2e
  JS (playwright):    --filter → --grep
  Python (pytest):    --filter → -k "p1 or p2"

Anything after '--' is passed verbatim to the underlying runner as extra args.

Examples:
  codefly test service                           # run all tests
  codefly test service --filter TestAuth         # run tests matching TestAuth
  codefly test service --filter Auth --filter API   # OR: TestAuth or TestAPI
  codefly test service --suite e2e               # run e2e suite (Playwright, etc.)
  codefly test service --target ./pkg/business   # scope to a package/dir
  codefly test service -- --shard 1/2            # pass --shard 1/2 to runner`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := common.SignalContext(ctx)
		defer stop()

		cli.Init()
		cli.RegisterCleanup(services.ClearAgents)

		// Anything after `--` becomes extra_args. cobra puts it in args
		// past ArgsLenAtDash().
		dashAt := cmd.ArgsLenAtDash()
		if dashAt >= 0 && dashAt < len(args) {
			testExtraArgs = append(testExtraArgs, args[dashAt:]...)
			args = args[:dashAt]
		}

		workspace, module, service := common.LoadRequired(ctx, args)
		serviceName := resources.WithUnique(service).Unique()
		isHeadless := headless || !term.IsTerminal(int(os.Stdout.Fd()))

		var flow *orchestration.Flow
		// testErr is the run/RPC error (init failure, crash, non-success exit).
		// It is the authoritative signal for the exit code.
		var testErr error

		if isHeadless {
			fmt.Printf("[codefly] Testing service %s (headless mode)\n", serviceName)
			flow, testErr = initRunService(ctx, workspace, module, service)
			if testErr == nil {
				testErr = common.WithHeartbeat(ctx, "running tests for "+serviceName, func() error {
					return testService(ctx, flow)
				})
			}
		} else {
			logCh := tui.NewLogChannel()
			cli.SuppressOutput()

			tuiErr := tui.RunServiceTUI(serviceName, logCh, func(t *tui.ServiceTUI) {
				t.SendState(serviceName, tui.StateLoading)

				flow, testErr = initRunService(ctx, workspace, module, service)
				if testErr != nil {
					t.SendError(testErr)
					return
				}

				t.SendState(serviceName, tui.StateTesting)

				testErr = testService(ctx, flow)
				if testErr != nil {
					t.SendError(testErr)
					return
				}

				t.SendDone(nil)
			})
			if tuiErr != nil && testErr == nil {
				testErr = tuiErr
			}
			// TUI suppressed stdout; restore it before printing the report.
			cli.RestoreOutput()
		}

		// Render the structured test report (counts + failed cases with
		// captured output) whenever the agent returned one — on success and
		// failure alike. This is the agent's Test RPC structured response.
		resp := flow.OriginTestResponse()
		if resp != nil {
			fmt.Println(orchestration.RenderTestReport(resp))
		}

		_ = stopService(ctx, flow)

		// Exit non-zero if the run errored OR the structured result is not a
		// pass. Previously this always called cli.Exit() (os.Exit(0)), so a
		// failing test suite reported success to CI.
		failed := testErr != nil || (resp != nil && !orchestration.TestSucceeded(resp))
		if failed {
			if testErr != nil {
				cli.ErrorChain(testErr, "tests failed for %s", serviceName)
			} else {
				cli.Error("tests failed for %s", serviceName)
			}
			cli.ExitError()
			return
		}
		fmt.Printf("[codefly] Tests passed for %s\n", serviceName)
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
	flow.WithTestRequest(buildTestRequest())

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
	w.Debug("Stopping services")
	err := flow.Stop()
	if err != nil {
		return w.Wrapf(err, "cannot stop service")
	}
	return nil
}

// buildTestRequest assembles a TestRequest from the CLI flags. Returns
// a zero-value request (run all tests, default settings) when no flags
// were supplied so existing callers see no behavioural change.
func buildTestRequest() *runtimev0.TestRequest {
	return &runtimev0.TestRequest{
		Target:    testTarget,
		Verbose:   testVerbose,
		Race:      testRace,
		Timeout:   testTimeout,
		Coverage:  testCoverage,
		Filters:   testFilters,
		Suite:     testSuite,
		ExtraArgs: testExtraArgs,
	}
}

func init() {
	ServiceCmd.Flags().StringVar(&runtimeContext, "runtime-context", "free", "Runtime context for the flow")
	ServiceCmd.Flags().StringVar(&scope, "scope", "", "Runtime scope (for testing encapsulation)")
	ServiceCmd.Flags().BoolVar(&initOnly, "init-only", false, "Initialize service only, i.e. without running it")
	ServiceCmd.Flags().BoolVar(&loadOnly, "load-only", false, "LoadRequired service only, i.e. without running it")
	ServiceCmd.Flags().BoolVar(&headless, "headless", false, "Run without TUI (auto-enabled when no TTY)")

	// Test filter flags — forwarded to the agent's Test RPC.
	ServiceCmd.Flags().StringVar(&testTarget, "target", "", "Package/directory scope (Go: ./pkg/foo, Python: tests/unit)")
	ServiceCmd.Flags().StringSliceVarP(&testFilters, "filter", "k", nil, "Name regex pattern (repeatable; OR-combined). -k mirrors pytest")
	ServiceCmd.Flags().StringVar(&testSuite, "suite", "", "Named suite: unit (default), integration, e2e, smoke")
	ServiceCmd.Flags().StringVar(&testTimeout, "timeout", "", "Per-test timeout, e.g. 30s")
	ServiceCmd.Flags().BoolVarP(&testVerbose, "verbose", "v", false, "Verbose runner output")
	ServiceCmd.Flags().BoolVar(&testRace, "race", false, "Run with race detector (Go)")
	ServiceCmd.Flags().BoolVar(&testCoverage, "coverage", false, "Run with coverage instrumentation")
}
