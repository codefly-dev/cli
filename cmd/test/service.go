package test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/cmd/run"
	"github.com/codefly-dev/cli/pkg/cli"
	clicomposition "github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/solutionrun"
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
	Short: "Run a service's configured tests through its agent",
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
	Args: serviceArgs,
	RunE: testServiceCommand,
}

// testServiceCommand is the shared test path: `test service` is this command,
// and `test solution` delegates to it with the resolved solution entry — the
// same construction `run solution` uses, so a solution is tested exactly the
// way its entry service is.
func testServiceCommand(cmd *cobra.Command, args []string) error {
	ctx, done := common.NewContext()
	defer done()

	ctx, stop := common.SignalContext(ctx)
	defer stop()

	cli.Init()
	defer services.ClearAgents()

	namingScopeExplicit = cmd.Flags().Changed("naming-scope")

	// Anything after `--` becomes extra_args. cobra puts it in args
	// past ArgsLenAtDash().
	request := buildTestRequest(nil)
	dashAt := cmd.ArgsLenAtDash()
	if dashAt >= 0 && dashAt < len(args) {
		request.ExtraArgs = append(request.ExtraArgs, args[dashAt:]...)
		args = args[:dashAt]
	}

	isHeadless := headless || !term.IsTerminal(int(os.Stdout.Fd()))
	loadRequired := common.LoadRequiredE
	if isHeadless {
		loadRequired = common.LoadRequiredNonInteractiveE
	}
	// Tests resolve composed pinned modules the same way a run does: a
	// workspace that composes a module by identity cannot load it as a local
	// checkout until the CLI has pulled it, so without this the load below
	// fails on every machine that has not run `run solution` first.
	if err := resolveTestPins(ctx); err != nil {
		return err
	}

	workspace, module, service, err := loadRequired(ctx, args)
	if err != nil {
		return fmt.Errorf("cannot load required service: %w", err)
	}
	serviceName := resources.WithUnique(service).Unique()

	// The same federation injections the run path derives: a solution tested
	// here must boot with CODEFLY__API_CONSUMES and its registration secrets
	// set, or its consumed routes stay unrouted under test but not under run.
	derived, derivedErr := solutionrun.DerivedRunInputs(ctx, workspace, module, service, serviceName)
	if derivedErr != nil {
		return derivedErr
	}
	// The derivation reports rather than prints, so its narration reaches a
	// terminal only from a caller that owns one. This is that caller.
	for _, note := range derived.Notes {
		if note.Warning {
			cli.Warning("%s", note.Message)
			continue
		}
		cli.Info("%s", note.Message)
	}

	var flow *orchestration.Flow
	// testErr is the run/RPC error (init failure, crash, non-success exit).
	// It is the authoritative signal for the exit code.
	var testErr error

	if isHeadless {
		fmt.Printf("[codefly] Testing service %s (headless mode)\n", serviceName)
		flow, testErr = initRunService(ctx, workspace, module, service, request, derived)
		if testErr == nil {
			testErr = common.WithHeartbeat(ctx, "running tests for "+serviceName, func() error {
				return testService(ctx, flow)
			})
		}
	} else {
		logCh := tui.NewLogChannel()
		cli.SuppressOutput()
		defer cli.RestoreOutput()

		tuiErr := tui.RunServiceTUI(serviceName, logCh, func(t *tui.ServiceTUI) {
			t.SendState(serviceName, tui.StateLoading)

			flow, testErr = initRunService(ctx, workspace, module, service, request, derived)
			if testErr != nil {
				t.SendError(testErr)
				t.SendDone(testErr)
				return
			}

			t.SendState(serviceName, tui.StateTesting)

			testErr = testService(ctx, flow)
			if testErr != nil {
				t.SendError(testErr)
				t.SendDone(testErr)
				return
			}

			t.SendDone(nil)
		})
		if tuiErr != nil && testErr == nil {
			testErr = tuiErr
		}
		cli.RestoreOutput()
	}

	// Render the structured test report (counts + failed cases with
	// captured output) whenever the agent returned one — on success and
	// failure alike. This is the agent's Test RPC structured response.
	var resp *runtimev0.TestResponse
	if flow != nil {
		resp = flow.OriginTestResponse()
	}
	if resp != nil {
		fmt.Println(orchestration.RenderTestReport(resp))
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	stopErr := stopService(shutdownCtx, flow)
	shutdownCancel()

	// Exit non-zero if the run errored OR the structured result is not a
	// pass. Previously this always called cli.Exit() (os.Exit(0)), so a
	// failing test suite reported success to CI.
	failed := testErr != nil || (resp != nil && !orchestration.TestSucceeded(resp))
	if failed {
		if testErr != nil {
			return errors.Join(fmt.Errorf("tests failed for %s: %w", serviceName, testErr), stopErr)
		}
		return errors.Join(fmt.Errorf("tests failed for %s", serviceName), stopErr)
	}
	if stopErr != nil {
		return fmt.Errorf("tests passed but service cleanup failed: %w", stopErr)
	}
	if flow != nil && flow.OriginTestSkipped() {
		fmt.Printf("[codefly] No tests for %s: agent advertises no test suites\n", serviceName)
		return nil
	}
	fmt.Printf("[codefly] Tests passed for %s\n", serviceName)
	return nil
}

// resolveTestPins materializes the workspace's composed pinned modules unless
// the caller already did, which pinsAlreadyResolved records.
func resolveTestPins(ctx context.Context) error {
	if pinsAlreadyResolved {
		return nil
	}
	return common.ResolvePinnedModulesForRun(ctx)
}

// testEnvironment resolves the environment this test flow runs in. It mirrors
// the run path: the workspace declaration wins, and --naming-scope applies to
// this invocation's copy only — never to the shared declaration. An explicitly
// empty scope clears a declared one; an absent flag keeps it.
func testEnvironment(workspace *resources.Workspace) (*resources.Environment, error) {
	env, err := orchestration.SelectEnvironment(workspace, environmentName)
	if err != nil {
		return nil, err
	}
	if namingScope != "" || namingScopeExplicit {
		env.NamingScope = namingScope
	}
	// An isolated invocation must not inherit the environment's declared scope.
	// Flow.WithIsolatedInvocation declines to generate an identity when a scope
	// is already set, so a workspace that declares one would leave two concurrent
	// tests sharing container names and runtime state directories while only
	// their ports differed — the collision isolation exists to prevent. env is a
	// deep copy, so clearing it never touches the shared declaration.
	if shouldIsolateInvocation(temporaryPorts, namingScopeExplicit) {
		env.NamingScope = ""
	}
	return env, nil
}

// verifyOutputEnvReachesOrigin refuses a test that asks for an exported runtime
// environment its origin will never produce. The fixture and the process
// overrides ride InitRequest, which reaches every service, but the exported
// environment is composed inside Start: only there, after the dependency
// barrier, are the origin's own endpoints and its dependencies' connections
// final. A suite that never starts the origin would get a file silently missing
// exactly those, which is worse than being told it cannot be written.
func verifyOutputEnvReachesOrigin(flow *orchestration.Flow, serviceName string) error {
	if outputEnv == "" || flow.OriginStartsForTest() || flow.OriginTestSkipped() {
		return nil
	}
	return fmt.Errorf(
		"%s runs its tests in dependency mode %s, which never starts the service under test, so --output-env cannot be written for it: its endpoints and dependency connections are composed at Start. Select a suite whose agent advertises START_STACK, or drop --output-env",
		serviceName, flow.TestDependencyModeName())
}

// shouldIsolateInvocation decides whether this test takes a generated identity
// for the resources it owns. An explicit --naming-scope, including an
// explicitly empty one, is the caller stating what it wants the run named —
// honored either way.
func shouldIsolateInvocation(temporaryPorts, namingScopeExplicit bool) bool {
	return temporaryPorts && !namingScopeExplicit
}

func serviceArgs(cmd *cobra.Command, args []string) error {
	positional := args
	if dashAt := cmd.ArgsLenAtDash(); dashAt >= 0 && dashAt <= len(args) {
		positional = args[:dashAt]
	}
	return cobra.MaximumNArgs(1)(cmd, positional)
}

func initRunService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, request *runtimev0.TestRequest, derived solutionrun.RunInputs) (*orchestration.Flow, error) {
	w := wool.Get(ctx).In("testService", wool.ThisField(resources.WithUnique(service)))
	defer w.Catch()

	if err := resources.ValidateRuntimeContext(runtimeContext); err != nil {
		return nil, w.NewError("Invalid runtime context: %s", runtimeContext)
	}

	env, err := testEnvironment(workspace)
	if err != nil {
		return nil, w.Wrap(err)
	}

	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.TestMode)
	if err != nil {
		return nil, w.Wrap(err)
	}
	flow.WithOutputSink(cli.NewOutputSink())
	flow.WithLoadOnly(loadOnly)
	flow.WithInitOnly(initOnly)
	flow.WithRuntimeContext(runtimeContext)
	flow.WithTestRequest(request)
	selectedFixture := orchestration.SelectedFixture(env, testFixture)
	if err = clicomposition.ValidateFixtureSelection(ctx, workspace, selectedFixture); err != nil {
		return nil, w.Wrap(err)
	}
	flow.WithFixture(selectedFixture)
	flow.WithOutputEnv(outputEnv)
	flow.WithOverrides(derived.Overrides)
	flow.WithWorkspaceConfigurationValues(derived.WorkspaceConfigurations)
	flow.WithTemporaryPorts(temporaryPorts)
	if shouldIsolateInvocation(temporaryPorts, namingScopeExplicit) {
		if invocation := flow.WithIsolatedInvocation(); invocation != "" {
			cli.Info(
				"isolated invocation %s: ephemeral ports, and every agent, container and runtime state directory this test owns is named under that scope",
				invocation,
			)
		}
	}
	resolvedProfile, err := workspace.ResolveRunProfile(ctx, profile, resources.RunProfile{ExcludeDependencies: excludeDependencies})
	if err != nil {
		return nil, w.Wrap(err)
	}
	if err = flow.WithRunProfile(resolvedProfile); err != nil {
		return nil, w.Wrap(err)
	}

	// Return the flow even when init fails: InitManagers spawns agents
	// incrementally (and Load can fail after they're live), so a partial failure
	// leaves live runners the caller must tear down via stopService(). Handing
	// back a nil flow here silently orphaned them — their process groups then
	// survived until the next run's reaper.
	err = flow.InitManagers(ctx)
	if err != nil {
		return flow, w.Wrap(err)
	}
	err = flow.Load(ctx)
	if err != nil {
		return flow, w.Wrap(err)
	}
	// Load resolved the origin's dependency mode, so this is the earliest point
	// the flow can tell whether the exported environment can be composed at
	// all. Skipped when nothing is going to run anyway.
	if !loadOnly && !initOnly {
		if err = verifyOutputEnvReachesOrigin(flow, resources.WithUnique(service).Unique()); err != nil {
			return flow, err
		}
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
func buildTestRequest(extraArgs []string) *runtimev0.TestRequest {
	return &runtimev0.TestRequest{
		Target:    testTarget,
		Verbose:   testVerbose,
		Race:      testRace,
		Timeout:   testTimeout,
		Coverage:  testCoverage,
		Filters:   append([]string(nil), testFilters...),
		Suite:     testSuite,
		ExtraArgs: append([]string(nil), extraArgs...),
	}
}

func init() {
	bindSharedTestFlags(ServiceCmd)
}

// bindSharedTestFlags registers every flag the test verbs take, bound to the
// package vars testServiceCommand reads. Both `test service` and `test solution`
// run that one path, so they register through this one function: hand-listing
// them per command is what left `test solution` without --race, --coverage,
// --init-only and --load-only.
func bindSharedTestFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&runtimeContext, "runtime-context", "free", "Runtime context for the flow")
	cmd.Flags().StringVar(&testFixture, "fixture", "", "Fixture override (defaults to the selected Codefly environment)")
	cmd.Flags().BoolVar(&initOnly, "init-only", false, "Initialize service only, i.e. without running it")
	cmd.Flags().BoolVar(&loadOnly, "load-only", false, "LoadRequired service only, i.e. without running it")
	cmd.Flags().BoolVar(&headless, "headless", false, "Run without TUI (auto-enabled when no TTY)")
	cmd.Flags().StringVar(&environmentName, "env", orchestration.LocalEnvironmentName, "Workspace environment to test in")
	cmd.Flags().StringVar(&profile, "profile", "", "Named workspace run profile")
	cmd.Flags().StringSliceVar(&excludeDependencies, "exclude-dependency", nil, "Exclude optional dependency services from the test (repeatable, e.g. infra/temporal)")
	cmd.Flags().StringVar(&outputEnv, "output-env", "", "Write one service's full SDK/runtime environment to an owner-only file")
	cmd.Flags().StringVar(&namingScope, "naming-scope", "", run.NamingScopeUsage)
	cmd.Flags().BoolVar(&temporaryPorts, "temporary-ports", true, run.TemporaryPortsUsage)

	// Test filter flags — forwarded to the agent's Test RPC.
	cmd.Flags().StringVar(&testTarget, "target", "", "Package/directory scope (Go: ./pkg/foo, Python: tests/unit)")
	cmd.Flags().StringSliceVarP(&testFilters, "filter", "k", nil, "Name regex pattern (repeatable; OR-combined). -k mirrors pytest")
	cmd.Flags().StringVar(&testSuite, "suite", "", "Named suite: unit (default), integration, e2e, smoke")
	cmd.Flags().StringVar(&testTimeout, "timeout", "", "Per-test timeout, e.g. 30s")
	cmd.Flags().BoolVarP(&testVerbose, "verbose", "v", false, "Verbose runner output")
	cmd.Flags().BoolVar(&testRace, "race", false, "Run with race detector (Go)")
	cmd.Flags().BoolVar(&testCoverage, "coverage", false, "Run with coverage instrumentation")
}
