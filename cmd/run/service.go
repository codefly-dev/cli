package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
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
	Short: "Run a service",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		// Ignore SIGHUP so the dev stack survives when the invoking shell,
		// terminal, or parent agent (Claude Code's Bash tool, a closed
		// Ghostty tab, etc.) hangs up. Without this, a parent-session
		// teardown cascades SIGHUP through the pgroup and kills every
		// agent + user binary mid-startup.
		signal.Ignore(syscall.SIGHUP)

		// Catch SIGINT (Ctrl-C) AND SIGTERM (kill, container shutdown).
		// os.Kill / SIGKILL cannot be caught, so listing it was a noop bug —
		// it gave the false impression the parent would clean up on `kill <pid>`,
		// but `kill` sends SIGTERM, which fell through and orphaned every plugin.
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()

		cli.Init()
		cli.RegisterCleanup(services.ClearAgents)

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
		}

		// Ryuk-adapted container sweep: remove any codefly-labeled Docker
		// containers whose owning CLI is dead. Same semantics as the pgid
		// sweep but for Docker-mode agents, which can't participate in
		// pgid tracking (process groups are namespaced inside containers).
		if err := dockerrun.ReapStaleContainers(ctx); err != nil {
			cli.Warning("stale container sweep failed: %v", err)
		}

		var workspace *resources.Workspace
		var module *resources.Module
		var service *resources.Service

		if servicePath != "" {
			workspace, module, service = common.LoadWithServicePathOverride(ctx, servicePath)
		} else {
			workspace, module, service = common.LoadRequired(ctx, args)
		}

		common.WithSilence(ctx, workspace, silent)

		if withCLIServer {
			// Propagate --naming-scope into the server's port derivation.
			// The test SDK (WithDependencies) appends the naming scope to
			// the workspace name when deriving CLIServerPort, so the spawned
			// CLI MUST use the same derivation or the client connects to a
			// port nobody's listening on — the documented cli-server ready
			// flake.
			server, err := web.NewServer(web.ServerData{Workspace: workspace, NamingScope: namingScope})
			cli.ExitOnError(err, "cannot create web server")
			go func() {
				_ = server.Start(ctx)
			}()
		}

		serviceName := resources.WithUnique(service).Unique()

		// Auto-detect headless: no TTY or explicit --headless flag
		isHeadless := headless || withCLIServer || !isTerminal()

		var flow *orchestration.Flow

		// stopFresh tears down whatever the flow started, using a FRESH context
		// (ctx/runCtx are cancelled by the time we shut down) with a generous
		// timeout so docker stop + agent shutdown run to completion. Used on
		// every exit path — including failures — so a partially-started flow
		// never orphans agents or containers.
		stopFresh := func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = stopService(shutdownCtx, flow)
			shutdownCancel()
		}

		// runFailed records whether the run ended in a failure (init/start error
		// or a post-readiness runtime failure) so the process exits non-zero.
		// Previously every path fell through to cli.Exit() (os.Exit(0)).
		runFailed := false

		if isHeadless {
			// Headless mode: plain log output, no TUI.
			// Works in: MCP, Claude Code, CI, Docker, pipes, scripts.
			//
			// phase reports a lifecycle milestone using the SAME marker (">>")
			// and vocabulary (tui.ServiceState.String()) as the core/tui
			// aggregate layer, so headless and interactive runs share one form —
			// ">> svc: Loading → Starting → Running" — instead of the old ad-hoc
			// "[codefly] svc: ..." colon form that collided with the TUI's ">>".
			phase := func(state tui.ServiceState) {
				fmt.Printf("%s %s: %s\n", cli.MarkerMilestone, serviceName, state)
			}

			fmt.Printf("[codefly] %s\n", cli.Legend())
			phase(tui.StateLoading)
			var err error
			flow, err = initRunService(ctx, workspace, module, service)
			if err != nil {
				cli.ErrorChain(err, "cannot initialize service %s", serviceName)
				stopFresh() // tear down anything Init partially started
				cli.ExitError()
				return
			}
			phase(tui.StateStarting)
			err = common.WithHeartbeat(ctx, "still starting "+serviceName, func() error {
				return runService(ctx, flow)
			})
			if err != nil {
				cli.ErrorChain(err, "cannot start service %s", serviceName)
				stopFresh() // tear down partially-started dependencies
				cli.ExitError()
				return
			}
			phase(tui.StateRunning)

			if withCLIServer {
				// Keep running with CLI server
			}

			// Wait for SIGINT OR a runner-level failure (e.g. user binary
			// os.Exit, plugin agent crash). Without the failure case we'd
			// idle forever with dead children — the orphan-process bug.
			select {
			case <-ctx.Done():
			case f := <-flow.Failures():
				cli.Error("service failure: %s", f.Error())
				runFailed = true
			}
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
			var runErr error
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

				var err error
				t.SendState(serviceName, tui.StateLoading)
				flow, err = initRunService(runCtx, workspace, module, service)
				if err != nil {
					// Keep the full error: initRunService returns w.NewError
					// (unwrapped) for an invalid runtime context, so Unwrap
					// returned nil and silently swallowed the failure.
					runErr = err
					t.SendError(runErr)
					t.SendDone(runErr) // quit the TUI instead of hanging on the error
					return
				}

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
				case f := <-flow.Failures():
					runErr = f
					t.SendError(f)
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
				cli.Error("TUI error: %v", tuiErr)
			}

			// TUI exited (quit/Done). Cancel the run context so flow.Start
			// unwinds, then wait for the startFn goroutine to finish before we
			// tear down — this also establishes happens-before on `flow`.
			runCancel()
			<-finished
			if runErr != nil {
				cli.ErrorChain(runErr, "service %s stopped with an error", serviceName)
				runFailed = true
			}
		}

		stopFresh()
		if runFailed {
			cli.ExitError()
			return
		}
		cli.Exit()
	},
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

// resolveRuntimeContext turns the "free" runtime context (the default, meaning
// "codefly picks the best available backend") into a concrete one:
//   - Docker engine running        → free (the Docker path, unchanged behavior)
//   - Docker down but nix available → nix  (Docker-free local dev)
//   - neither                       → free (startup surfaces a clear Docker error)
//
// An explicit request (native/container/nix) is honored as-is — only "free"
// auto-resolves. CODEFLY__RUNTIME_CONTEXT feeds the flag default (see init), so
// `CODEFLY__RUNTIME_CONTEXT=nix go test ...` forces nix without a flag.
// defaultRuntimeContext seeds the --runtime-context flag default from
// CODEFLY__RUNTIME_CONTEXT when set (so dependency stacks spawned by the SDK
// inherit it), otherwise "free" (codefly auto-picks Docker-or-nix at run time).
func defaultRuntimeContext() string {
	if rc := strings.TrimSpace(os.Getenv("CODEFLY__RUNTIME_CONTEXT")); rc != "" {
		return rc
	}
	return resources.RuntimeContextFree
}

func resolveRuntimeContext(ctx context.Context, requested string) string {
	if requested != resources.RuntimeContextFree {
		return requested
	}
	w := wool.Get(ctx).In("run.resolveRuntimeContext")
	if dockerrun.DockerEngineRunning(ctx) {
		return resources.RuntimeContextFree
	}
	if runnersbase.CheckNixInstalled() && runnersbase.IsNixSupported() {
		w.Info("Docker engine not running — falling back to the nix runtime (Docker-free)")
		return resources.RuntimeContextNix
	}
	w.Warn("Docker engine not running and nix unavailable — continuing with Docker (startup will fail with a Docker error)")
	return resources.RuntimeContextFree
}

func initRunService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) (*orchestration.Flow, error) {
	w := wool.Get(ctx).In("runService", wool.ThisField(resources.WithUnique(service)))
	// Catch panic
	defer w.Catch()

	if err := resources.ValidateRuntimeContext(runtimeContext); err != nil {
		return nil, w.NewError("Invalid runtime context: %s", runtimeContext)
	}
	runtimeContext = resolveRuntimeContext(ctx, runtimeContext)

	env := resources.LocalEnvironment()
	// Setup optional naming namingScope
	env.NamingScope = namingScope

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

	flow.WithLoadOnly(loadOnly)
	flow.WithInitOnly(initOnly)
	flow.WithOutputEnv(outputEnv)
	flow.WithStandAlone(standAlone)
	flow.WithExcludeRoot(excludeRoot)
	flow.WithRuntimeContext(runtimeContext)
	flow.WithFixture(fixture)
	overrides, err := parseSetOverrides(setOverrides)
	if err != nil {
		return nil, w.Wrap(err)
	}
	flow.WithOverrides(overrides)
	flow.WithRemotes(remoteServices)
	excludedDependencies, err := resolveExcludedDependencies(ctx, workspace, excludeDependencies)
	if err != nil {
		return nil, w.Wrap(err)
	}
	flow.WithExcludedDependencies(excludedDependencies)

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

func resolveExcludedDependencies(ctx context.Context, workspace *resources.Workspace, inputs []string) ([]string, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, input := range inputs {
		for _, raw := range strings.Split(input, ",") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			ref, err := workspace.FindUniqueServiceAndModuleByName(ctx, raw)
			if err != nil {
				return nil, fmt.Errorf("resolving --exclude-dependency %q: %w", raw, err)
			}
			unique := ref.Unique()
			if seen[unique] {
				continue
			}
			seen[unique] = true
			out = append(out, unique)
		}
	}
	return out, nil
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
	ServiceCmd.Flags().StringVar(&runtimeContext, "runtime-context", defaultRuntimeContext(), "Runtime context for the flow (native/container/nix/free; free auto-picks Docker-or-nix)")
	ServiceCmd.Flags().StringVar(&namingScope, "naming-scope", "", "Runtime namingScope (for testing encapsulation)")
	ServiceCmd.Flags().BoolVar(&standAlone, "stand-alone", false, "Begin service as standalone, i.e. without its dependencies")
	ServiceCmd.Flags().StringVar(&servicePath, "service-path", "", "Path to the service")
	ServiceCmd.Flags().StringVar(&outputEnv, "output-env", "", "Output environment variables")
	ServiceCmd.Flags().BoolVar(&excludeRoot, "exclude-root", false, "Exclude root service")
	ServiceCmd.Flags().BoolVar(&initOnly, "init-only", false, "Initialize service only, i.e. without running it")
	ServiceCmd.Flags().BoolVar(&loadOnly, "load-only", false, "LoadRequired service only, i.e. without running it")
	ServiceCmd.Flags().StringSliceVar(&silent, "silent", nil, "Silence services in CLI output")
	ServiceCmd.Flags().StringSliceVar(&excludeDependencies, "exclude-dependency", nil, "Exclude optional dependency services from the run (repeatable, e.g. infra/temporal)")
	ServiceCmd.Flags().StringVar(&fixture, "fixture", "", "Fixture to use for the service")
	ServiceCmd.Flags().StringSliceVar(&setOverrides, "set", nil, "Per-service runtime env override (repeatable), e.g. --set warden:CODEFLY__FIXTURE=dogfood")
	ServiceCmd.Flags().StringSliceVar(&remotes, "remote", nil, "Remote services")
	ServiceCmd.Flags().BoolVar(&headless, "headless", false, "Run without TUI (auto-enabled when no TTY, e.g. MCP, CI, pipes)")
}
