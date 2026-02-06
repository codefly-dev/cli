package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/daemon"
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
		// Build the arguments for the child process:
		// "run service" + whatever the user passed after "--"
		childArgs := []string{"run", "service"}
		childArgs = append(childArgs, args...)

		pid, err := daemon.Start(childArgs)
		if err != nil {
			cli.Error("Cannot start daemon: %v", err)
			cli.Exit()
		}

		logPath, _ := daemon.LogFile()
		cli.Header(1, "Daemon started (PID %d)", pid)
		fmt.Printf("  Logs: %s\n", logPath)
		fmt.Printf("  Stop: codefly daemon stop\n")
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
			cli.Exit()
		}
		if !status.Running {
			fmt.Println("Daemon is not running.")
			return
		}

		fmt.Printf("Stopping daemon (PID %d)...\n", status.PID)
		if err := daemon.Stop(); err != nil {
			cli.Error("Cannot stop daemon: %v", err)
			cli.Exit()
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
			cli.Exit()
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
			cli.Exit()
		}
		f, err := os.Open(logPath)
		if os.IsNotExist(err) {
			fmt.Println("No daemon logs found.")
			return
		}
		if err != nil {
			cli.Error("Cannot open log file: %v", err)
			cli.Exit()
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
					select {}
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

func init() {
	daemonLogsCmd.Flags().BoolVarP(&logFollow, "follow", "f", false, "Follow log output")
	daemonLogsCmd.Flags().IntVarP(&logTail, "tail", "n", 0, "Show last N lines")

	DaemonCmd.AddCommand(daemonStartCmd)
	DaemonCmd.AddCommand(daemonStopCmd)
	DaemonCmd.AddCommand(daemonStatusCmd)
	DaemonCmd.AddCommand(daemonLogsCmd)
}
