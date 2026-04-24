package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/web"
	"github.com/codefly-dev/core/resources"
	runnersbase "github.com/codefly-dev/core/runners/base"
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
		// Ryuk-adapted container sweep: remove any codefly-labeled Docker
		// containers whose owning CLI is dead. Same semantics as the pgid
		// sweep but for Docker-mode agents, which can't participate in
		// pgid tracking (process groups are namespaced inside containers).
		if err := runnersbase.ReapStaleContainers(ctx); err != nil {
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

		if isHeadless {
			// Headless mode: plain log output, no TUI
			// Works in: MCP, Claude Code, CI, Docker, pipes, scripts
			fmt.Printf("[codefly] Starting service %s (headless mode)\n", serviceName)
			var err error
			flow, err = initRunService(ctx, workspace, module, service)
			if err != nil {
				cli.Error("init failed: %v", errors.Unwrap(err))
				cli.Exit()
				return
			}
			fmt.Printf("[codefly] Service initialized, starting...\n")
			err = runService(ctx, flow)
			if err != nil {
				cli.Error("run failed: %v", err)
				cli.Exit()
				return
			}
			fmt.Printf("[codefly] Service %s is running\n", serviceName)

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
			}
		} else {
			// Interactive mode: TUI
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

				t.SendState(serviceName, tui.StateStarting)

				err = runService(ctx, flow)
				if err != nil {
					t.SendError(err)
					return
				}

				t.SendReady(serviceName, 0)
				select {
				case <-ctx.Done():
				case f := <-flow.Failures():
					t.SendError(f)
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

// isTerminal checks if stdout is connected to a terminal.
// Returns false in: MCP, Claude Code, CI, Docker, piped output.
func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func initRunService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) (*orchestration.Flow, error) {
	w := wool.Get(ctx).In("runService", wool.ThisField(resources.WithUnique(service)))
	// Catch panic
	defer w.Catch()

	if err := resources.ValidateRuntimeContext(runtimeContext); err != nil {
		return nil, w.NewError("Invalid runtime context: %s", runtimeContext)
	}

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
	fmt.Printf("[DEBUG CLI] fixture=%q\n", fixture)
	flow.WithFixture(fixture)
	flow.WithRemotes(remoteServices)

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

func parseRemote(workspace *resources.Workspace, remotes []string) ([]*orchestration.Remote, error) {
	var out []*orchestration.Remote
	// Remote should be unique-ish:env
	for _, remote := range remotes {
		tokens := strings.Split(remote, ":")
		if len(tokens) != 2 {
			return nil, errors.New("Remote should be in the format: service:env")
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
	w.Info("Stopping services")
	err := flow.Stop()
	if err != nil {
		return w.Wrapf(err, "cannot stop service")
	}
	return nil
}

func init() {
	ServiceCmd.Flags().BoolVar(&withCLIServer, "cli-server", false, "Start CLI server")
	ServiceCmd.Flags().StringVar(&runtimeContext, "runtime-context", "free", "Runtime context for the flow")
	ServiceCmd.Flags().StringVar(&namingScope, "naming-scope", "", "Runtime namingScope (for testing encapsulation)")
	ServiceCmd.Flags().BoolVar(&standAlone, "stand-alone", false, "Begin service as standalone, i.e. without its dependencies")
	ServiceCmd.Flags().StringVar(&servicePath, "service-path", "", "Path to the service")
	ServiceCmd.Flags().StringVar(&outputEnv, "output-env", "", "Output environment variables")
	ServiceCmd.Flags().BoolVar(&excludeRoot, "exclude-root", false, "Exclude root service")
	ServiceCmd.Flags().BoolVar(&initOnly, "init-only", false, "Initialize service only, i.e. without running it")
	ServiceCmd.Flags().BoolVar(&loadOnly, "load-only", false, "LoadRequired service only, i.e. without running it")
	ServiceCmd.Flags().StringSliceVar(&silent, "silent", nil, "Silence services in CLI output")
	ServiceCmd.Flags().StringVar(&fixture, "fixture", "", "Fixture to use for the service")
	ServiceCmd.Flags().StringSliceVar(&remotes, "remote", nil, "Remote services")
	ServiceCmd.Flags().BoolVar(&headless, "headless", false, "Run without TUI (auto-enabled when no TTY, e.g. MCP, CI, pipes)")
}
