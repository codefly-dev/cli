package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/daemon"
	"github.com/codefly-dev/cli/pkg/gateway"
	runnersbase "github.com/codefly-dev/core/runners/base"
	"github.com/spf13/cobra"
)

// DaemonCmd is the top-level "daemon" command group.
var DaemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Manage the codefly background service",
}

// --- daemon start ---

var daemonStartCmd = &cobra.Command{
	Use:   "start [-- flags passed to 'run service']",
	Short: "Start services in the background",
	Long: `Starts a detached codefly daemon that runs your services.

Any flags after "--" are forwarded to the underlying "run service" command.

Examples:
  codefly daemon start
  codefly daemon start -- --runtime-context nix
  codefly daemon start -- -d --service-path ./my-svc`,
	Run: func(cmd *cobra.Command, args []string) {
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
		} else {
			// Default: "run service" + whatever the user passed after "--"
			childArgs = append(childArgs, "run", "service")
			childArgs = append(childArgs, args...)
		}

		pid, err := daemon.Start(childArgs)
		if err != nil {
			cli.Error("Cannot start daemon: %v", err)
			cli.ExitError()
		}

		logPath, _ := daemon.LogFile()
		cli.Header(1, "Daemon started (PID %d)", pid)
		fmt.Printf("  Logs: %s\n", logPath)
		fmt.Printf("  Stop: codefly daemon stop\n")
		if startGateway {
			fmt.Printf("  Gateway: 127.0.0.1:%d\n", gatewayPort)
		}
	},
}

// --- daemon stop ---

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running daemon",
	Run: func(cmd *cobra.Command, args []string) {
		status, err := daemon.GetStatus()
		if err != nil {
			cli.Error("Cannot check daemon status: %v", err)
			cli.ExitError()
		}
		if !status.Running {
			fmt.Println("Daemon is not running.")
			return
		}

		fmt.Printf("Stopping daemon (PID %d)...\n", status.PID)
		if err := daemon.Stop(); err != nil {
			cli.Error("Cannot stop daemon: %v", err)
			cli.ExitError()
		}
		fmt.Println("Daemon stopped.")
	},
}

// --- daemon status ---

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Check if the daemon is running",
	Run: func(cmd *cobra.Command, args []string) {
		status, err := daemon.GetStatus()
		if err != nil {
			cli.Error("Cannot check daemon status: %v", err)
			cli.ExitError()
		}
		if status.Running {
			fmt.Printf("Daemon is running (PID %d)\n", status.PID)
			fmt.Printf("  Logs: %s\n", status.LogPath)
		} else {
			fmt.Println("Daemon is not running.")
		}
	},
}

// --- daemon logs ---

var (
	logFollow bool
	logTail   int
)

var daemonLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Show daemon log output",
	Run: func(cmd *cobra.Command, args []string) {
		logPath, err := daemon.LogFile()
		if err != nil {
			cli.Error("Cannot determine log path: %v", err)
			cli.ExitError()
		}
		f, err := os.Open(logPath)
		if os.IsNotExist(err) {
			fmt.Println("No daemon logs found.")
			return
		}
		if err != nil {
			cli.Error("Cannot open log file: %v", err)
			cli.ExitError()
		}
		defer f.Close()

		if logTail > 0 {
			printTail(f, logTail)
		} else {
			io.Copy(os.Stdout, f)
		}

		if logFollow {
			// Simple follow: keep reading as new data arrives
			for {
				n, _ := io.Copy(os.Stdout, f)
				if n == 0 {
					// Small sleep to avoid busy-wait
					time.Sleep(250 * time.Millisecond)
				}
			}
		}
	},
}

// printTail prints the last n lines of a file.
func printTail(f *os.File, n int) {
	// Read entire file (daemon logs should be manageable in size)
	data, err := io.ReadAll(f)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	start := len(lines) - n
	if start < 0 {
		start = 0
	}
	for _, line := range lines[start:] {
		fmt.Println(line)
	}
}

// --- daemon gateway (internal: runs the gateway gRPC server in foreground) ---

var (
	gatewayDir  string
	gatewayPort int
)

var daemonGatewayCmd = &cobra.Command{
	Use:    "gateway",
	Short:  "Run the Mind Gateway gRPC server (foreground)",
	Long:   "Starts the Mind Gateway gRPC server in the foreground. Typically invoked by 'daemon start --gateway' or by Mind automatically.",
	Hidden: false,
	Run: func(cmd *cobra.Command, args []string) {
		// Catch SIGTERM too: `daemon stop` sends SIGTERM for graceful shutdown.
		// Listening only for SIGINT meant SIGTERM killed the gateway by default
		// disposition, skipping the deferred RemovePortFile and the server's
		// graceful unwind — leaving a stale port file read as a live gateway.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		absDir := gatewayDir
		if absDir == "" {
			absDir = "."
		}

		// CODEFLY_GATEWAY_HOST lets a container expose the gateway over the
		// network (set it to 0.0.0.0). Empty keeps the local-only 127.0.0.1
		// default — no behavior change for normal local runs.
		srv, err := gateway.NewServer(gateway.Config{
			WorkDir: absDir,
			Port:    gatewayPort,
			Host:    os.Getenv("CODEFLY_GATEWAY_HOST"),
		})
		if err != nil {
			cli.Error("Cannot create gateway server: %v", err)
			cli.ExitError()
		}

		// Clean up port file on exit.
		defer gateway.RemovePortFile()

		if err := srv.Serve(ctx); err != nil {
			cli.Error("Gateway server error: %v", err)
			cli.ExitError()
		}
	},
}

// --- daemon restart ---

var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the running daemon (stop then start)",
	Run: func(cmd *cobra.Command, args []string) {
		// Stop if running.
		status, err := daemon.GetStatus()
		if err != nil {
			cli.Error("Cannot check daemon status: %v", err)
			cli.ExitError()
		}
		if status.Running {
			fmt.Printf("Stopping daemon (PID %d)...\n", status.PID)
			if err := daemon.Stop(); err != nil {
				cli.Error("Cannot stop daemon: %v", err)
				cli.ExitError()
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
			cli.Error("Cannot start daemon: %v", err)
			cli.ExitError()
		}

		logPath, _ := daemon.LogFile()
		cli.Header(1, "Daemon restarted (PID %d)", pid)
		fmt.Printf("  Logs: %s\n", logPath)
		fmt.Printf("  Stop: codefly daemon stop\n")
	},
}

// --- daemon monitor ---

var (
	monitorWatch bool
	monitorKill  bool
)

var daemonMonitorCmd = &cobra.Command{
	Use:   "monitor",
	Short: "Monitor codefly processes for CPU/memory issues",
	Long: `Checks all codefly-related processes (agents, server, Neo4j) for:
- High CPU usage (>200% for 2 consecutive checks → auto-kill agents)
- High memory usage (>512MB → warning)
- Orphaned agent processes (>3 → warning)

Use -w/--watch for continuous monitoring.
Use --kill-orphans to clean up orphaned agent processes.`,
	Run: func(cmd *cobra.Command, args []string) {
		cfg := daemon.DefaultMonitorConfig()

		if monitorKill {
			// Delegate to the pgid-aware reaper. It only kills process groups
			// whose owning CLI is dead (and escalates SIGTERM→SIGKILL with a
			// grace period). The previous implementation killed EVERY go-grpc /
			// go-generic agent by name with no liveness check — which SIGKILLed
			// the agents of any concurrent `codefly run`, re-creating the exact
			// orphan/zombie state this command exists to clean up.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := runnersbase.ReapStaleProcessGroups(ctx); err != nil {
				cli.Error("Cannot reap orphaned process groups: %v", err)
				cli.ExitError()
			}
			fmt.Println("Reaped orphaned agent process groups (live runs left untouched).")
			return
		}

		if monitorWatch {
			fmt.Println("Monitoring codefly processes (Ctrl+C to stop)...")
			daemon.RunMonitorLoop(cfg)
			return
		}

		// One-shot check
		result, err := daemon.Monitor(cfg)
		if err != nil {
			cli.Error("Monitor failed: %v", err)
			cli.ExitError()
		}
		fmt.Print(daemon.FormatStatus(result))
	},
}

// --- daemon start --gateway ---

var startGateway bool

func init() {
	daemonLogsCmd.Flags().BoolVarP(&logFollow, "follow", "f", false, "Follow log output")
	daemonLogsCmd.Flags().IntVarP(&logTail, "tail", "n", 0, "Show last N lines")

	daemonGatewayCmd.Flags().StringVar(&gatewayDir, "dir", ".", "Working directory containing mind.yaml")
	daemonGatewayCmd.Flags().IntVar(&gatewayPort, "port", 50051, "gRPC listen port")

	daemonStartCmd.Flags().BoolVar(&startGateway, "gateway", false, "Start the Mind Gateway gRPC server instead of running services")
	daemonStartCmd.Flags().StringVar(&gatewayDir, "dir", ".", "Working directory for gateway (requires --gateway)")
	daemonStartCmd.Flags().IntVar(&gatewayPort, "port", 50051, "gRPC port for gateway (requires --gateway)")

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
