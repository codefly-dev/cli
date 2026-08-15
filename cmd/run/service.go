package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/engine"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/web"
	"github.com/codefly-dev/core/resources"
	runnersbase "github.com/codefly-dev/core/runners/base"
	dockerrun "github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/tui"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Start a service locally with its dependency graph",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runServiceCommand,
}

func runServiceCommand(cmd *cobra.Command, args []string) (returnErr error) {
	ctx, done := common.NewContext()
	defer done()

	// Ignore SIGHUP so the dev stack survives when the invoking shell,
	// terminal, or parent agent (Claude Code's Bash tool, a closed
	// Ghostty tab, etc.) hangs up. Without this, a parent-session
	// teardown cascades SIGHUP through the pgroup and kills every
	// agent + user binary mid-startup.
	signal.Ignore(syscall.SIGHUP)
	defer signal.Reset(syscall.SIGHUP)

	// Catch SIGINT (Ctrl-C) AND SIGTERM (kill, container shutdown).
	// os.Kill / SIGKILL cannot be caught, so listing it was a noop bug —
	// it gave the false impression the parent would clean up on `kill <pid>`,
	// but `kill` sends SIGTERM, which fell through and orphaned every plugin.
	ctx, stop := common.SignalContext(ctx)
	defer stop()
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	cli.Init()
	defer services.ClearAgents()

	namingScopeExplicit = cmd.Flags().Changed("naming-scope")

	// Auto-detect headless: no TTY or explicit --headless flag.
	isHeadless := headless || withCLIServer || !isTerminal()

	// In interactive mode the TUI takes over the terminal further down. Start
	// recording codefly's narration now so the pre-flow work below (stale-process
	// reaping, workspace load) is replayed into the log pane when the TUI opens,
	// instead of being painted to a terminal the alt screen then erases — which
	// is why the interactive log pane used to start mid-init. Capture tees rather
	// than diverts, so a load failure that prints-and-exits here stays visible.
	if !isHeadless {
		cli.StartCapture()
	}

	// Reap any process groups orphaned by a prior ungraceful CLI exit
	// (parent SIGKILL, terminal force-closed). Without this, zombie
	// trees from a previous `codefly run` survive and hold ports,
	// making the next run appear to fork-bomb or fail port binding.
	if err := runnersbase.ReapStaleProcessGroups(ctx); err != nil {
		cli.Warning("stale process-group sweep failed: %v", err)
	}
	// SDK / test runs (`codefly run --cli-server`) spawn per-run dependency
	// containers under a unique naming scope that are never reused. Mark
	// them ephemeral BEFORE provisioning so the sweep can reap them even
	// while running — otherwise a killed test leaks its Neo4j/Postgres and
	// they accumulate (the OrbStack-memory-blowup bug). Must be set before
	// the flow creates any container.
	if withCLIServer {
		dockerrun.SetEphemeralContainers(true)
		defer dockerrun.SetEphemeralContainers(false)
	}

	// Ryuk-adapted container sweep: remove any codefly-labeled Docker
	// containers whose owning CLI is dead. Same semantics as the pgid
	// sweep but for Docker-mode agents, which can't participate in
	// pgid tracking (process groups are namespaced inside containers).
	if shouldSweepStaleContainers(runtimeContext) {
		if err := dockerrun.ReapStaleContainers(ctx); err != nil {
			cli.Warning("stale container sweep failed: %v", err)
		}
	}

	var workspace *resources.Workspace
	var module *resources.Module
	var service *resources.Service

	var err error
	if servicePath != "" {
		workspace, module, service, err = common.LoadWithServicePathOverrideE(ctx, servicePath)
	} else {
		workspace, module, service, err = loadRequiredServiceForRun(ctx, args, isHeadless)
	}
	if err != nil {
		return fmt.Errorf("cannot load required service: %w", err)
	}

	if err := common.WithSilenceE(ctx, workspace, silent); err != nil {
		return err
	}

	serviceName := resources.WithUnique(service).Unique()

	var flow *orchestration.Flow

	// flowManager, when the CLI server is up, is how the server's RPCs
	// resolve this run's flow — never through a process-global, which would
	// alias a second flow started elsewhere in the same process.
	var flowManager *engine.FlowManager
	registerFlow := func() {
		if flowManager != nil && flow != nil {
			_ = flowManager.Register(serviceName, flow)
		}
	}

	var serverResult chan error
	defer func() {
		cancelRun()
		if serverResult != nil {
			if err := <-serverResult; err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("CLI server failed: %w", err))
			}
		}
	}()
	if withCLIServer {
		flowManager = engine.NewFlowManager()
		// Propagate --naming-scope into the server's port derivation.
		// The test SDK (WithDependencies) appends the naming scope to
		// the workspace name when deriving CLIServerPort, so the spawned
		// CLI MUST use the same derivation or the client connects to a
		// port nobody's listening on — the documented cli-server ready
		// flake. Deliberately the raw flag, NOT the resolved environment's
		// naming scope: the SDK client only knows the scope it passed, so
		// a workspace-declared scope must affect service naming only,
		// never this port contract.
		server, err := web.NewServer(web.ServerData{Workspace: workspace, NamingScope: namingScope, Flows: flowManager})
		if err != nil {
			return fmt.Errorf("cannot create web server: %w", err)
		}
		serverResult = make(chan error, 1)
		go func() {
			err := server.Start(ctx)
			serverResult <- err
			if err != nil {
				cancelRun()
			}
		}()
	}

	// stopFresh tears down whatever the flow started, using a FRESH context
	// (ctx/runCtx are cancelled by the time we shut down) with a generous
	// timeout so docker stop + agent shutdown run to completion. Used on
	// every exit path — including failures — so a partially-started flow
	// never orphans agents or containers.
	stopFresh := func() error {
		if flowManager != nil {
			flowManager.Release(serviceName, flow)
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		err := stopService(shutdownCtx, flow)
		shutdownCancel()
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, stopFresh()) }()

	var runErr error

	if isHeadless {
		// Headless mode: plain log output, no TUI.
		// Works in: MCP, Claude Code, CI, Docker, pipes, scripts.
		//
		// phase reports a lifecycle milestone using the SAME marker (">>")
		// and vocabulary (tui.ServiceState.String()) as the core/tui
		// aggregate layer, so headless and interactive runs share one form —
		// ">> svc: Loading → Starting → Running" — instead of the old ad-hoc
		// "[codefly] svc: ..." colon form that collided with the TUI's ">>".
		// milestones de-dupes exact per-service repeats so the action loop
		// and the readiness poller can't print the same ">>" line twice.
		milestones := cli.NewMilestoneEmitter()
		runStart := time.Now()
		phase := func(state tui.ServiceState) {
			var elapsed time.Duration
			if state == tui.StateRunning {
				elapsed = time.Since(runStart)
			}
			milestones.Emit(serviceName, cli.Milestone(serviceName, state.String(), 0, elapsed))
		}

		fmt.Printf("[codefly] %s\n", cli.Legend())
		phase(tui.StateLoading)
		var err error
		flow, err = initRunService(ctx, workspace, module, service)
		registerFlow()
		if err != nil {
			return fmt.Errorf("cannot initialize service %s: %w", serviceName, err)
		}
		// Print each dependency's lifecycle (Loading→Init→Starting→Running,
		// or Failed) using the same ">>" milestone form as the origin, so a
		// stalled dependency (e.g. vault waiting on its health probe) is
		// visible in headless/MCP/CI logs instead of silent until timeout.
		// printReady prints a dependency's "Running on :port" milestone
		// exactly once, whichever source observes it first — the action-loop
		// emit below or the readiness poller further down — so the loop's
		// eventual StateRunning doesn't duplicate a line the poller printed.
		var promotedMu sync.Mutex
		promoted := map[string]bool{}
		printReady := func(service string, port int) {
			promotedMu.Lock()
			already := promoted[service]
			promoted[service] = true
			promotedMu.Unlock()
			if already {
				return
			}
			milestones.Emit(service, cli.Milestone(service, tui.StateRunning.String(), port, 0))
		}

		flow.WithStateListener(func(service string, state tui.ServiceState, port int) {
			if service == serviceName {
				return
			}
			if state == tui.StateRunning {
				printReady(service, port)
				return
			}
			milestones.Emit(service, cli.Milestone(service, state.String(), 0, 0))
		})

		phase(tui.StateStarting)

		// Drive dependency readiness off a real probe concurrently with the
		// blocking run. The action loop can't emit a dependency's StateRunning
		// until its own (blocking) Start returns, so while it is parked in the
		// origin's go compile an already-listening dependency would otherwise
		// stay silent. Mirrors the interactive TUI's ticker; printReady dedupes
		// against the loop's eventual emit.
		pollCtx, pollCancel := context.WithCancel(ctx)
		var pollWg sync.WaitGroup

		// In run mode runService blocks for the LIFETIME of the stack —
		// the playbook action loop only returns on cancellation or a
		// failure — so the "still starting" heartbeat below must be
		// stopped by READINESS, not by runService returning. Without
		// this, a healthy long-lived headless run (e.g. a bench under
		// sdk.WithDependencies) prints "still starting <svc>… Ns"
		// forever even though every dependency has been Running for
		// hours. markRunning emits the Running milestone exactly once,
		// whichever observer sees readiness first, and silences the
		// heartbeat.
		hbCtx, hbCancel := context.WithCancel(ctx)
		defer hbCancel()
		var runningOnce sync.Once
		markRunning := func() {
			runningOnce.Do(func() {
				phase(tui.StateRunning)
				hbCancel()
			})
		}
		pollWg.Go(func() {
			pollPromoted := map[string]bool{}
			ticker := time.NewTicker(150 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-pollCtx.Done():
					return
				case <-ticker.C:
					flow.PromoteReachable(serviceName, pollPromoted, printReady)
					if hbCtx.Err() == nil && flow.Ready(pollCtx) {
						markRunning()
					}
				}
			}
		})

		// hbCtx only scopes the heartbeat TICKER; runService still runs
		// under the real ctx.
		err = common.WithHeartbeat(hbCtx, "still starting "+serviceName, func() error {
			return runService(ctx, flow)
		})
		pollCancel()
		pollWg.Wait()
		if err != nil {
			// Attribute the failure to the service that actually failed (e.g. a
			// dependency that couldn't start), not always to the origin.
			if culprit, phase, ok := flow.FailedService(); ok && culprit != serviceName {
				return fmt.Errorf("service %s failed during %s (while starting %s): %w", culprit, phase, serviceName, err)
			}
			return fmt.Errorf("cannot start service %s: %w", serviceName, err)
		}
		if !shouldWaitForRun(loadOnly, initOnly) {
			phase(tui.StateStopped)
			return nil
		}
		// runningOnce keeps this from double-printing when the poller already
		// announced readiness.
		markRunning()

		if withCLIServer {
			// Keep running with CLI server
		}

		<-ctx.Done()
	} else {
		// Interactive mode: TUI
		logCh := tui.NewLogChannel()
		cli.SuppressOutput()

		// The TUI quits on a Ctrl-C KEYPRESS (raw mode → byte 0x03 →
		// tea.Quit), NOT a SIGINT — so the process-level signal context is
		// NOT cancelled when the user quits the TUI. We therefore drive the
		// flow with our own cancelable context and cancel it explicitly once
		// the TUI exits, so flow.Start's action loop unwinds before we stop.
		runCtx, runCancel := context.WithCancel(ctx)
		defer runCancel()

		// runErr is written by the startFn goroutine and read only after
		// <-finished, which the deferred close synchronizes — so the read
		// (and the read of `flow` below) is race-free.
		finished := make(chan struct{})

		// The async update check started in cli.Init() lands at an arbitrary
		// time. Routed to stderr (its default) it fires mid-render and leaves
		// a stale duplicate of the status bar (#57); route it into the TUI as
		// a one-shot log line above the live status bar instead. Set up the
		// capture before the TUI starts so no notice ever reaches stderr while
		// it owns the terminal, buffering until the ServiceTUI exists.
		var noticeMu sync.Mutex
		var noticeTUI *tui.ServiceTUI
		var pendingNotice string
		restoreNotice := cli.CaptureUpdateNotice(func(msg string) {
			noticeMu.Lock()
			defer noticeMu.Unlock()
			if noticeTUI != nil {
				noticeTUI.SendLog(wool.WARN, "codefly", msg)
			} else {
				pendingNotice = msg
			}
		})
		defer restoreNotice()

		tuiErr := tui.RunServiceTUI(serviceName, logCh, func(t *tui.ServiceTUI) {
			defer close(finished)

			noticeMu.Lock()
			noticeTUI = t
			if pendingNotice != "" {
				t.SendLog(wool.WARN, "codefly", pendingNotice)
				pendingNotice = ""
			}
			noticeMu.Unlock()

			// Replay everything narrated before the TUI owned the screen
			// (stale-process reaping, workspace load) so the log pane starts
			// where a headless run does instead of mid-init.
			for _, line := range cli.DrainCapture() {
				t.SendLog(line.Level, "codefly", line.Message)
			}

			// Route codefly's own narration (e.g. "Handling <frontend> with
			// these dependent services: …") into the TUI log pane while it
			// owns the screen — otherwise it goes to stdout and the alt
			// screen overwrites it, leaving a blank "Loading" for the whole
			// init phase. Cleared when the flow returns so the post-TUI
			// error report prints to the real terminal again.
			cli.SetOutputSink(func(level wool.Loglevel, msg string) {
				t.SendLog(level, "codefly", msg)
			})
			defer cli.SetOutputSink(nil)

			var err error
			t.SendState(serviceName, tui.StateLoading)
			flow, err = initRunService(runCtx, workspace, module, service)
			registerFlow()
			if err != nil {
				// Keep the full error: initRunService returns w.NewError
				// (unwrapped) for an invalid runtime context, so Unwrap
				// returned nil and silently swallowed the failure.
				runErr = err
				t.SendError(runErr)
				t.SendDone(runErr) // quit the TUI instead of hanging on the error
				return
			}

			// Drive per-dependency live status from the orchestrator so each
			// dependency shows its own Loading→Init→Starting→Running (and any
			// stall) instead of hiding behind the origin's spinner. The origin
			// is already driven by the control flow here, so skip it to avoid
			// double milestones.
			flow.WithStateListener(func(service string, state tui.ServiceState, port int) {
				if service == serviceName {
					return
				}
				switch state {
				case tui.StateRunning:
					t.SendReady(service, port)
				case tui.StateFailed:
					t.SendFailed(service)
				default:
					t.SendState(service, state)
				}
			})
			t.SendPlan(flow.OrderedServiceUniques())

			t.SendState(serviceName, tui.StateStarting)

			// flow.Start runs the playbook action loop and only returns
			// once runCtx is cancelled (or start fails), so run it in the
			// background and watch flow.Ready to flip "Starting" → "Running".
			startErr := make(chan error, 1)
			go func() { startErr <- runService(runCtx, flow) }()

			// drainStart waits for the background flow.Start goroutine to
			// return, so it is never still running when stopService begins.
			drainStart := func() { runCancel(); runErr = orFirst(runErr, <-startErr) }

			ticker := time.NewTicker(150 * time.Millisecond)
			defer ticker.Stop()

			// While the action loop is blocked inside a long phase (the origin's
			// go compile being the usual culprit), it can't emit a dependency's
			// StateRunning even after that dependency is actually listening — so
			// an up-and-ready postgres keeps showing as "slow". Promote each
			// dependency to ready from a real readiness probe instead, so the
			// live status reflects what's actually up rather than where the
			// synchronous loop happens to be parked. `promoted` is touched only
			// by this goroutine.
			promoted := map[string]bool{}

			for !flow.Ready(runCtx) {
				select {
				case <-runCtx.Done():
					drainStart()
					t.SendDone(runErr)
					return
				case err := <-startErr:
					// flow.Start returned before readiness: a start failure
					// or a clean early exit (init-only / immediate stop).
					runErr = err
					if err != nil {
						t.SendError(err)
					}
					t.SendDone(err) // quit instead of spinning on "Starting"
					return
				case <-ticker.C:
					flow.PromoteReachable(serviceName, promoted, t.SendReady)
				}
			}

			t.SendReady(serviceName, 0)
			// Tell the TUI which dependencies are running alongside the
			// origin, so the shutdown view names exactly what gets torn down
			// on quit (origin + these — none stay alive).
			if _, deps := flow.ManagedServices(); len(deps) > 0 {
				t.SendStopPlan(deps)
			}
			select {
			case <-runCtx.Done():
			case err := <-startErr:
				// flow.Start returned on its own after readiness — already
				// drained, so don't call drainStart (it would block).
				runErr = err
				if err != nil {
					t.SendError(err)
				}
				t.SendDone(err)
				return
			}
			drainStart()
			t.SendDone(runErr)
		})
		if tuiErr != nil {
			runErr = orFirst(runErr, fmt.Errorf("TUI error: %w", tuiErr))
		}

		// TUI exited (quit/Done). Cancel the run context so flow.Start
		// unwinds, then wait for the startFn goroutine to finish before we
		// tear down — this also establishes happens-before on `flow`.
		runCancel()
		<-finished
		cli.RestoreOutput()
	}

	if runErr != nil {
		return fmt.Errorf("service %s stopped with an error: %w", serviceName, runErr)
	}
	return nil
}

// shouldSweepStaleContainers keeps explicit Local/Nix runs independent of the
// Docker control plane. A free run may select Docker and an explicit container
// run requires it, so those startup paths retain orphan cleanup.
func shouldSweepStaleContainers(selectedRuntime string) bool {
	return selectedRuntime != resources.RuntimeContextNative && selectedRuntime != resources.RuntimeContextNix
}

// loadRequiredServiceForRun keeps terminal access aligned with the execution
// mode selected at the command boundary. SDK, CI, and explicit headless runs
// must fail on ambiguous workspace context instead of opening /dev/tty from a
// background process group.
func loadRequiredServiceForRun(ctx context.Context, args []string, isHeadless bool) (*resources.Workspace, *resources.Module, *resources.Service, error) {
	if isHeadless {
		return common.LoadRequiredNonInteractiveE(ctx, args)
	}
	return common.LoadRequiredE(ctx, args)
}

func shouldWaitForRun(loadOnly, initOnly bool) bool {
	return !loadOnly && !initOnly
}

// orFirst returns a when it is non-nil, otherwise b. Used to keep the first
// (more meaningful) error when draining a secondary error source.
func orFirst(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// isTerminal checks if stdout is connected to a terminal.
// Returns false in: MCP, Claude Code, CI, Docker, piped output.
func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// defaultRuntimeContext seeds the --runtime-context flag default from
// CODEFLY__RUNTIME_CONTEXT when set (so dependency stacks spawned by the SDK
// inherit it), otherwise "free" (codefly picks the first backend advertised
// as available by each service agent at run time).
func defaultRuntimeContext() string {
	if rc := strings.TrimSpace(os.Getenv("CODEFLY__RUNTIME_CONTEXT")); rc != "" {
		return rc
	}
	return resources.RuntimeContextFree
}

// probeDocker reports whether the Docker engine is reachable and, for the
// fallback error/warning, names the docker context/endpoint being probed.
// core's dockerrun clients resolve the active docker context internally for the
// actual connection; resolveDockerHost is a read-only mirror used only to make
// the message actionable ("docker context "orbstack" → unix:///…").
func probeDocker(ctx context.Context) orchestration.DockerStatus {
	name, endpoint := resolveDockerHost(ctx)
	return orchestration.DockerStatus{
		Running:  dockerrun.DockerEngineRunning(ctx),
		Context:  name,
		Endpoint: endpoint,
	}
}

// resolveDockerHost resolves the Docker endpoint the way the docker CLI does —
// an explicit DOCKER_HOST wins, otherwise the active `docker context` endpoint —
// for user-facing messaging only (it does not mutate the environment). Returns
// the resolved context name (empty when DOCKER_HOST is set) and endpoint.
func resolveDockerHost(ctx context.Context) (contextName, endpoint string) {
	if h := strings.TrimSpace(os.Getenv("DOCKER_HOST")); h != "" {
		return "", h
	}
	name := strings.TrimSpace(os.Getenv("DOCKER_CONTEXT"))
	if name == "" {
		if out, err := exec.CommandContext(ctx, "docker", "context", "show").Output(); err == nil {
			name = strings.TrimSpace(string(out))
		}
	}
	if name == "" || name == "default" {
		return name, ""
	}
	out, err := exec.CommandContext(ctx, "docker", "context", "inspect", name, "--format", "{{ .Endpoints.docker.Host }}").Output()
	if err != nil {
		return name, ""
	}
	return name, strings.TrimSpace(string(out))
}

// runEnvironment resolves the selected environment this run executes in. The
// workspace declaration (secret backends, configuration profile, naming
// policy, …) wins over defaults, and the --naming-scope override applies to
// this invocation's copy only — never to the shared declaration.
// An explicitly passed empty --naming-scope clears a declared scope; an
// absent flag keeps it.
func runEnvironment(workspace *resources.Workspace) (*resources.Environment, error) {
	env, err := orchestration.SelectEnvironment(workspace, environmentName)
	if err != nil {
		return nil, err
	}
	if namingScope != "" || namingScopeExplicit {
		env.NamingScope = namingScope
	}
	return env, nil
}

// newRunFlow selects the environment and wires the run flow up to — but
// excluding — agent creation, so this call site stays testable without
// spawning agent processes.
func newRunFlow(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) (*orchestration.Flow, error) {
	w := wool.Get(ctx).In("runService", wool.ThisField(resources.WithUnique(service)))

	if err := resources.ValidateRuntimeContext(runtimeContext); err != nil {
		return nil, w.NewError("Invalid runtime context: %s", runtimeContext)
	}

	env, err := runEnvironment(workspace)
	if err != nil {
		return nil, w.Wrap(err)
	}

	// Parse remote services
	remoteServices, err := parseRemote(workspace, remotes)
	if err != nil {
		return nil, w.Wrap(err)
	}
	w.Info("Running with remotes", wool.Field("remotes", remoteServices))

	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.RunMode)
	if err != nil {
		return nil, w.Wrap(err)
	}

	flow.WithOutputSink(cli.NewOutputSink())
	flow.WithLoadOnly(loadOnly)
	flow.WithInitOnly(initOnly)
	flow.WithOutputEnv(outputEnv)
	if outputEnvService != "" {
		if outputEnv == "" {
			return nil, w.NewError("--output-env-service requires --output-env")
		}
		if _, err := flow.ServiceFromUnique(outputEnvService); err != nil {
			return nil, w.Wrapf(err, "cannot select output environment service %q", outputEnvService)
		}
		flow.WithOutputEnvService(outputEnvService)
	}
	flow.WithStandAlone(standAlone)
	flow.WithExcludeRoot(excludeRoot)
	flow.WithRuntimeContext(runtimeContext)
	flow.WithTemporaryPorts(temporaryPorts)
	// Only the "free" default lets codefly pick an advertised backend, so only
	// then do we probe Docker (which shells out to the docker CLI). An explicit
	// context is honored as-is and needs no probe.
	if runtimeContext == resources.RuntimeContextFree {
		flow.WithStartDocker(startDocker)
		flow.WithDockerStatus(probeDocker(ctx))
	}
	flow.WithFixture(orchestration.SelectedFixture(env, fixture))
	overrides, err := parseSetOverrides(setOverrides)
	if err != nil {
		return nil, w.Wrap(err)
	}
	flow.WithOverrides(overrides)
	flow.WithRemotes(remoteServices)
	resolvedProfile, err := workspace.ResolveRunProfile(ctx, profile, resources.RunProfile{ExcludeDependencies: excludeDependencies})
	if err != nil {
		return nil, w.Wrap(err)
	}
	if err := flow.WithRunProfile(resolvedProfile); err != nil {
		return nil, w.Wrap(err)
	}
	return flow, nil
}

func initRunService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) (*orchestration.Flow, error) {
	w := wool.Get(ctx).In("runService", wool.ThisField(resources.WithUnique(service)))
	// Catch panic
	defer w.Catch()

	flow, err := newRunFlow(ctx, workspace, module, service)
	if err != nil {
		return nil, err
	}

	// Return the flow even when init fails: InitManagers spawns agents
	// incrementally (and Load can fail after they're live), so a partial failure
	// leaves live runners the caller must tear down via stopFresh(). Handing back
	// a nil flow here silently orphaned them — their process groups then survived
	// until the next run's reaper.
	err = flow.InitManagers(ctx)
	if err != nil {
		return flow, w.Wrap(err)
	}
	err = flow.Load(ctx)
	if err != nil {
		return flow, w.Wrap(err)
	}
	return flow, nil
}

// parseSetOverrides turns repeatable --set entries of the form
// "service:KEY=VAL" into a serviceName -> KEY -> VAL map. It splits on the
// first ':' (so the service prefix is unambiguous) and the first '=' (so the
// value may contain '=' or ':'). Malformed entries are rejected with a clear error.
func parseSetOverrides(entries []string) (map[string]map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(map[string]map[string]string)
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		service, kv, ok := strings.Cut(entry, ":")
		if !ok || service == "" {
			return nil, fmt.Errorf("--set %q is malformed: expected <service>:KEY=VAL", entry)
		}
		key, value, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("--set %q is malformed: expected <service>:KEY=VAL", entry)
		}
		if out[service] == nil {
			out[service] = make(map[string]string)
		}
		out[service][key] = value
	}
	return out, nil
}

func parseRemote(workspace *resources.Workspace, remotes []string) ([]*orchestration.Remote, error) {
	var out []*orchestration.Remote
	// Remote should be unique-ish:env
	for _, remote := range remotes {
		tokens := strings.Split(remote, ":")
		if len(tokens) != 2 {
			return nil, errors.New("remote should be in the format: service:env")
		}
		serviceWithModule, err := resources.ParseServiceWithOptionalModule(tokens[0])
		if err != nil {
			return nil, err
		}
		// Need to check if we know this environment
		env := &resources.Environment{Name: tokens[1]}
		out = append(out, &orchestration.Remote{ServiceWithModule: serviceWithModule, Environment: env})
	}
	return out, nil
}

func runService(ctx context.Context, flow *orchestration.Flow) error {
	// Catch panic
	w := wool.Get(ctx).In("runService")
	defer w.Catch()
	err := flow.Start(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot start service")
	}
	return nil
}

func stopService(ctx context.Context, flow *orchestration.Flow) error {
	// Catch panic
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

func init() {
	ServiceCmd.Flags().BoolVar(&withCLIServer, "cli-server", false, "Start CLI server")
	ServiceCmd.Flags().StringVar(&runtimeContext, "runtime-context", defaultRuntimeContext(), "Runtime context for the flow (native/container/nix/free; free picks the first advertised backend)")
	ServiceCmd.Flags().StringVar(&namingScope, "naming-scope", "", "Runtime namingScope (for testing encapsulation)")
	ServiceCmd.Flags().BoolVar(&temporaryPorts, "temporary-ports", false, "Allocate OS-probed temporary ports for this flow (advanced; intended for SDK-managed tests)")
	ServiceCmd.Flags().BoolVar(&standAlone, "stand-alone", false, "Begin service as standalone, i.e. without its dependencies")
	ServiceCmd.Flags().StringVar(&servicePath, "service-path", "", "Path to the service")
	ServiceCmd.Flags().StringVar(&outputEnv, "output-env", "", "Write one service's full SDK/runtime environment to an owner-only file")
	ServiceCmd.Flags().StringVar(&outputEnvService, "output-env-service", "", "Service whose runtime environment to export (module/service; defaults to the root service)")
	ServiceCmd.Flags().BoolVar(&excludeRoot, "exclude-root", false, "Exclude root service")
	ServiceCmd.Flags().BoolVar(&initOnly, "init-only", false, "Initialize service only, i.e. without running it")
	ServiceCmd.Flags().BoolVar(&loadOnly, "load-only", false, "LoadRequired service only, i.e. without running it")
	ServiceCmd.Flags().StringVar(&environmentName, "env", orchestration.LocalEnvironmentName, "Workspace environment to run")
	ServiceCmd.Flags().StringSliceVar(&silent, "silent", nil, "Silence services in CLI output")
	ServiceCmd.Flags().StringVar(&profile, "profile", "", "Named workspace run profile")
	ServiceCmd.Flags().StringSliceVar(&excludeDependencies, "exclude-dependency", nil, "Exclude optional dependency services from the run (repeatable, e.g. infra/temporal)")
	ServiceCmd.Flags().StringVar(&fixture, "fixture", "", "Fixture override (defaults to the selected Codefly environment)")
	ServiceCmd.Flags().StringSliceVar(&setOverrides, "set", nil, "Per-service runtime env override (repeatable), e.g. --set warden:CODEFLY__FIXTURE=dogfood")
	ServiceCmd.Flags().StringSliceVar(&remotes, "remote", nil, "Remote services")
	ServiceCmd.Flags().BoolVar(&headless, "headless", false, "Run without TUI (auto-enabled when no TTY, e.g. MCP, CI, pipes)")
	ServiceCmd.Flags().BoolVar(&startDocker, "start-docker", true, "Auto-start a local Docker engine (OrbStack/Docker Desktop/colima/…) if a service needs Docker and it isn't running; --start-docker=false to disable")
}
