package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/daemon"
	"github.com/codefly-dev/cli/pkg/gateway"
	runnersbase "github.com/codefly-dev/core/runners/base"
	"github.com/spf13/cobra"
)

// DaemonCmd is the top-level "daemon" command group.
var DaemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Run and inspect Codefly services in the background",
}

// --- daemon start ---

var daemonStartCmd = &cobra.Command{
	Use:   "start [-- flags passed to 'run service']",
	Short: "Run workspace services in a detached background daemon",
	Long: `Starts a detached codefly daemon that runs your services.

Any flags after "--" are forwarded to the underlying "run service" command.

Examples:
  codefly daemon start
  codefly daemon start -- --runtime-context nix
  codefly daemon start -- -d --service-path ./my-svc`,
	RunE: func(cmd *cobra.Command, args []string) error {
		var childArgs []string

		// Propagate global log-level flags to the child process so the
		// re-exec'd gateway (or service) runs with the same verbosity.
		if trace {
			childArgs = append(childArgs, "--trace")
		} else if debug {
			childArgs = append(childArgs, "--debug")
		}

		if startGateway {
			// Start the gateway gRPC server as a daemon.
			childArgs = append(childArgs, "daemon", "gateway",
				"--dir", gatewayDir,
				"--port", fmt.Sprintf("%d", gatewayPort),
			)
			executionArgs, err := gatewayExecution.childArgs()
			if err != nil {
				return err
			}
			childArgs = append(childArgs, executionArgs...)
		} else {
			if gatewayExecution.enabled || gatewayExecution.hasConfiguration() {
				return fmt.Errorf("governed execution flags require daemon start --gateway")
			}
			// Default: "run service" + whatever the user passed after "--"
			childArgs = append(childArgs, "run", "service")
			childArgs = append(childArgs, args...)
		}

		pid, err := daemon.Start(childArgs)
		if err != nil {
			return fmt.Errorf("cannot start daemon: %w", err)
		}

		logPath, _ := daemon.LogFile()
		cli.Header(1, "Daemon started (PID %d)", pid)
		fmt.Printf("  Logs: %s\n", logPath)
		fmt.Printf("  Stop: codefly daemon stop\n")
		if startGateway {
			fmt.Printf("  Gateway: 127.0.0.1:%d\n", gatewayPort)
		}
		return nil
	},
}

// --- daemon stop ---

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the active background daemon and its services",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		status, err := daemon.GetStatus()
		if err != nil {
			return fmt.Errorf("cannot check daemon status: %w", err)
		}
		if !status.Running {
			fmt.Println("Daemon is not running.")
			return nil
		}

		fmt.Printf("Stopping daemon (PID %d)...\n", status.PID)
		if err := daemon.Stop(); err != nil {
			return fmt.Errorf("cannot stop daemon: %w", err)
		}
		fmt.Println("Daemon stopped.")
		return nil
	},
}

// --- daemon status ---

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report whether the Codefly daemon is running",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		status, err := daemon.GetStatus()
		if err != nil {
			return fmt.Errorf("cannot check daemon status: %w", err)
		}
		if status.Running {
			fmt.Printf("Daemon is running (PID %d)\n", status.PID)
			fmt.Printf("  Logs: %s\n", status.LogPath)
		} else {
			fmt.Println("Daemon is not running.")
		}
		return nil
	},
}

// --- daemon logs ---

var (
	logFollow bool
	logTail   int
)

var daemonLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Read or follow output from the background daemon",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		logPath, err := daemon.LogFile()
		if err != nil {
			return fmt.Errorf("cannot determine log path: %w", err)
		}
		f, err := os.Open(logPath)
		if os.IsNotExist(err) {
			fmt.Println("No daemon logs found.")
			return nil
		}
		if err != nil {
			return fmt.Errorf("cannot open log file: %w", err)
		}
		defer f.Close()

		if logTail > 0 {
			if err := printTail(f, logTail); err != nil {
				return fmt.Errorf("read daemon log tail: %w", err)
			}
		} else {
			if _, err := io.Copy(os.Stdout, f); err != nil {
				return fmt.Errorf("read daemon log: %w", err)
			}
		}

		if logFollow {
			// Follow: keep reading as new data arrives. Catch SIGINT/SIGTERM so
			// Ctrl-C (or `kill`) exits gracefully instead of forcing the user to
			// force-kill a loop that otherwise never returns.
			ctx, stop := common.SignalContext(cmd.Context())
			defer stop()

			for {
				select {
				case <-ctx.Done():
					return nil
				default:
				}
				n, err := io.Copy(os.Stdout, f)
				if err != nil && err != io.EOF {
					return fmt.Errorf("log read error: %w", err)
				}
				if n == 0 {
					// Small sleep to avoid busy-wait
					time.Sleep(250 * time.Millisecond)
				}
			}
		}
		return nil
	},
}

// printTail prints the last n lines of a file.
func printTail(f *os.File, n int) error {
	// Read entire file (daemon logs should be manageable in size)
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	start := len(lines) - n
	if start < 0 {
		start = 0
	}
	for _, line := range lines[start:] {
		fmt.Println(line)
	}
	return nil
}

// --- daemon gateway (internal: runs the gateway gRPC server in foreground) ---

var (
	gatewayDir       string
	gatewayPort      int
	gatewayExecution gatewayExecutionOptions
)

var daemonGatewayCmd = &cobra.Command{
	Use:    "gateway",
	Short:  "Serve Codefly workspace operations to Mind over gRPC",
	Long:   "Starts the Mind Gateway gRPC server in the foreground. Typically invoked by 'daemon start --gateway' or by Mind automatically.",
	Hidden: false,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Catch SIGTERM too: `daemon stop` sends SIGTERM for graceful shutdown.
		// Listening only for SIGINT meant SIGTERM killed the gateway by default
		// disposition, skipping the deferred RemovePortFile and the server's
		// graceful unwind — leaving a stale port file read as a live gateway.
		ctx, stop := common.SignalContext(cmd.Context())
		defer stop()

		absDir := gatewayDir
		if absDir == "" {
			absDir = "."
		}

		execution, err := gatewayExecution.open(ctx, absDir)
		if err != nil {
			return fmt.Errorf("cannot configure governed execution: %w", err)
		}
		if execution != nil {
			defer func() {
				if closeErr := execution.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "[gateway] close governed execution: %v\n", closeErr)
				}
			}()
		}

		// CODEFLY_GATEWAY_HOST lets a container expose the gateway over the
		// network (set it to 0.0.0.0). Non-loopback binds require a bearer
		// token plus a TLS certificate/key. Supplying a client CA additionally
		// requires verified client certificates (mTLS). Empty host keeps the
		// local-only 127.0.0.1 default.
		var executionRecorder gateway.ExecutionRecorder
		var executionDispatcher gateway.ExecutionDispatcher
		if execution != nil {
			executionRecorder = execution.Recorder
			executionDispatcher = execution.Dispatcher
		}
		srv, err := gateway.NewServer(gateway.Config{
			WorkDir:             absDir,
			Port:                gatewayPort,
			Host:                os.Getenv("CODEFLY_GATEWAY_HOST"),
			Token:               os.Getenv("CODEFLY_GATEWAY_TOKEN"),
			TLSCertFile:         os.Getenv("CODEFLY_GATEWAY_TLS_CERT"),
			TLSKeyFile:          os.Getenv("CODEFLY_GATEWAY_TLS_KEY"),
			TLSClientCAFile:     os.Getenv("CODEFLY_GATEWAY_TLS_CLIENT_CA"),
			ExecutionRecorder:   executionRecorder,
			ExecutionDispatcher: executionDispatcher,
		})
		if err != nil {
			return fmt.Errorf("cannot create gateway server: %w", err)
		}

		// Clean up port file on exit.
		defer gateway.RemovePortFile()

		if err := srv.Serve(ctx); err != nil {
			return fmt.Errorf("gateway server error: %w", err)
		}
		return nil
	},
}

// --- daemon restart ---

var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the background daemon with its previous arguments",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Stop if running.
		status, err := daemon.GetStatus()
		if err != nil {
			return fmt.Errorf("cannot check daemon status: %w", err)
		}
		if status.Running {
			fmt.Printf("Stopping daemon (PID %d)...\n", status.PID)
			if err := daemon.Stop(); err != nil {
				return fmt.Errorf("cannot stop daemon: %w", err)
			}
			fmt.Println("Daemon stopped.")
		}

		// Start again.
		var childArgs []string
		if trace {
			childArgs = append(childArgs, "--trace")
		} else if debug {
			childArgs = append(childArgs, "--debug")
		}
		childArgs = append(childArgs, "run", "service")
		childArgs = append(childArgs, args...)

		pid, err := daemon.Start(childArgs)
		if err != nil {
			return fmt.Errorf("cannot start daemon: %w", err)
		}

		logPath, _ := daemon.LogFile()
		cli.Header(1, "Daemon restarted (PID %d)", pid)
		fmt.Printf("  Logs: %s\n", logPath)
		fmt.Printf("  Stop: codefly daemon stop\n")
		return nil
	},
}

// --- daemon monitor ---

var (
	monitorWatch bool
	monitorKill  bool
)

var daemonMonitorCmd = &cobra.Command{
	Use:   "monitor",
	Short: "Watch Codefly processes for CPU and memory pressure",
	Long: `Checks all codefly-related processes (agents, server, Neo4j) for:
- High CPU usage (>200% for 2 consecutive checks → auto-kill agents)
- High memory usage (>512MB → warning)
- Orphaned agent processes (>3 → warning)

Use -w/--watch for continuous monitoring.
Use --kill-orphans to clean up orphaned agent processes.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := daemon.DefaultMonitorConfig()

		if monitorKill {
			// Delegate to the pgid-aware reaper. It only kills process groups
			// whose owning CLI is dead (and escalates SIGTERM→SIGKILL with a
			// grace period). The previous implementation killed EVERY go-grpc /
			// go-generic agent by name with no liveness check — which SIGKILLed
			// the agents of any concurrent `codefly run`, re-creating the exact
			// orphan/zombie state this command exists to clean up.
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			if err := runnersbase.ReapStaleProcessGroups(ctx); err != nil {
				return fmt.Errorf("cannot reap orphaned process groups: %w", err)
			}
			fmt.Println("Reaped orphaned agent process groups (live runs left untouched).")
			return nil
		}

		if monitorWatch {
			fmt.Println("Monitoring codefly processes (Ctrl+C to stop)...")
			ctx, stop := common.SignalContext(cmd.Context())
			defer stop()
			return daemon.RunMonitorLoop(ctx, cfg)
		}

		// One-shot check
		result, err := daemon.Monitor(cfg)
		if err != nil {
			return fmt.Errorf("monitor failed: %w", err)
		}
		fmt.Print(daemon.FormatStatus(result))
		return nil
	},
}

// --- daemon start --gateway ---

var startGateway bool

func init() {
	daemonLogsCmd.Flags().BoolVarP(&logFollow, "follow", "f", false, "Follow log output")
	daemonLogsCmd.Flags().IntVarP(&logTail, "tail", "n", 0, "Show last N lines")

	daemonGatewayCmd.Flags().StringVar(&gatewayDir, "dir", ".", "Working directory containing mind.yaml")
	daemonGatewayCmd.Flags().IntVar(&gatewayPort, "port", 50051, "gRPC listen port")
	addGatewayExecutionFlags(daemonGatewayCmd)

	daemonStartCmd.Flags().BoolVar(&startGateway, "gateway", false, "Start the Mind Gateway gRPC server instead of running services")
	daemonStartCmd.Flags().StringVar(&gatewayDir, "dir", ".", "Working directory for gateway (requires --gateway)")
	daemonStartCmd.Flags().IntVar(&gatewayPort, "port", 50051, "gRPC port for gateway (requires --gateway)")
	addGatewayExecutionFlags(daemonStartCmd)

	daemonMonitorCmd.Flags().BoolVarP(&monitorWatch, "watch", "w", false, "Run continuously (every 30s)")
	daemonMonitorCmd.Flags().BoolVar(&monitorKill, "kill-orphans", false, "Kill orphaned agent processes")

	DaemonCmd.AddCommand(daemonStartCmd)
	DaemonCmd.AddCommand(daemonStopCmd)
	DaemonCmd.AddCommand(daemonRestartCmd)
	DaemonCmd.AddCommand(daemonStatusCmd)
	DaemonCmd.AddCommand(daemonLogsCmd)
	DaemonCmd.AddCommand(daemonGatewayCmd)
	DaemonCmd.AddCommand(daemonMonitorCmd)
}
