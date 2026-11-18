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
	"github.com/codefly-dev/cli/pkg/processgroup"
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
	RunE: func(_ *cobra.Command, args []string) error {
		var childArgs []string

		// Propagate global log-level flags to the child process so the
		// re-exec'd service runs with the same verbosity.
		if trace {
			childArgs = append(childArgs, "--trace")
		} else if debug {
			childArgs = append(childArgs, "--debug")
		}
		// "run service" + whatever the user passed after "--"
		childArgs = append(childArgs, "run", "service")
		childArgs = append(childArgs, args...)

		pid, err := daemon.Start(childArgs)
		if err != nil {
			return fmt.Errorf("cannot start daemon: %w", err)
		}

		logPath, _ := daemon.LogFile()
		cli.Header(1, "Daemon started (PID %d)", pid)
		fmt.Printf("  Logs: %s\n", logPath)
		fmt.Printf("  Stop: codefly daemon stop\n")
		return nil
	},
}

// --- daemon stop ---

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the active background daemon and its services",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
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
	RunE: func(_ *cobra.Command, _ []string) error {
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
	RunE: func(cmd *cobra.Command, _ []string) error {
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

// --- daemon restart ---

var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the background daemon with its previous arguments",
	RunE: func(_ *cobra.Command, args []string) error {
		// Stop if running.
		status, err := daemon.GetStatus()
		if err != nil {
			return fmt.Errorf("cannot check daemon status: %w", err)
		}
		if status.Running {
			fmt.Printf("Stopping daemon (PID %d)...\n", status.PID)
			if err = daemon.Stop(); err != nil {
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
	RunE: func(cmd *cobra.Command, _ []string) error {
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
			if err := processgroup.ReapStaleProcessGroups(ctx); err != nil {
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

func init() {
	daemonLogsCmd.Flags().BoolVarP(&logFollow, "follow", "f", false, "Follow log output")
	daemonLogsCmd.Flags().IntVarP(&logTail, "tail", "n", 0, "Show last N lines")

	daemonMonitorCmd.Flags().BoolVarP(&monitorWatch, "watch", "w", false, "Run continuously (every 30s)")
	daemonMonitorCmd.Flags().BoolVar(&monitorKill, "kill-orphans", false, "Kill orphaned agent processes")

	DaemonCmd.AddCommand(daemonStartCmd)
	DaemonCmd.AddCommand(daemonStopCmd)
	DaemonCmd.AddCommand(daemonRestartCmd)
	DaemonCmd.AddCommand(daemonStatusCmd)
	DaemonCmd.AddCommand(daemonLogsCmd)
	DaemonCmd.AddCommand(daemonMonitorCmd)
}
