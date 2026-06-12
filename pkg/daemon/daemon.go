// Package daemon manages the codefly background service process.
//
// The daemon re-execs the current binary with an internal flag and detaches
// from the terminal. The PID and log files live under ~/.codefly/.
package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	pidFileName = "daemon.pid"
	logFileName = "daemon.log"
)

// Paths returns the directory used for daemon state files.
func stateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	dir := filepath.Join(home, ".codefly")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("cannot create state dir: %w", err)
	}
	return dir, nil
}

// PIDFile returns the path to the PID file.
func PIDFile() (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, pidFileName), nil
}

// LogFile returns the path to the daemon log file.
func LogFile() (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, logFileName), nil
}

// WritePID writes the current process PID to the PID file.
func WritePID() error {
	path, err := PIDFile()
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)
}

// ReadPID reads the PID from the PID file. Returns 0 if the file doesn't exist.
func ReadPID() (int, error) {
	path, err := PIDFile()
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("cannot read PID file: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("invalid PID in file: %w", err)
	}
	return pid, nil
}

// RemovePID removes the PID file.
func RemovePID() error {
	path, err := PIDFile()
	if err != nil {
		return err
	}
	return os.Remove(path)
}

// IsRunning checks if a process with the given PID is still alive.
func IsRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks if the process exists without killing it.
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

// isOurDaemon verifies that the live process at pid is actually a codefly
// daemon, not an unrelated process that recycled the PID after our daemon
// exited. Without this, Stop() could SIGTERM/SIGKILL a stranger. Best-effort:
// when the process command can't be read (permissions, platform), it returns
// true so Stop still works on hosts where `ps` is unavailable — liveness alone
// is the pre-existing behavior.
func isOurDaemon(pid int) bool {
	if pid <= 0 {
		return false
	}
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return true // can't introspect — fall back to liveness-only
	}
	cmd := strings.ToLower(strings.TrimSpace(string(out)))
	if cmd == "" {
		return true
	}
	return strings.Contains(cmd, "codefly")
}

// Status returns the daemon's current state.
type Status struct {
	Running bool
	PID     int
	LogPath string
}

// GetStatus checks if the daemon is currently running.
func GetStatus() (*Status, error) {
	pid, err := ReadPID()
	if err != nil {
		return nil, err
	}
	logPath, _ := LogFile()
	return &Status{
		// Running only if the PID is alive AND is actually our daemon (guards
		// against a recycled PID being reported as a live daemon).
		Running: IsRunning(pid) && isOurDaemon(pid),
		PID:     pid,
		LogPath: logPath,
	}, nil
}

// Start launches the daemon as a detached background process.
// It re-execs the current binary with the given args, redirecting
// stdout and stderr to the log file.
func Start(args []string) (int, error) {
	// Check if already running
	status, err := GetStatus()
	if err != nil {
		return 0, err
	}
	if status.Running {
		return status.PID, fmt.Errorf("daemon is already running (PID %d)", status.PID)
	}

	logPath, err := LogFile()
	if err != nil {
		return 0, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("cannot open log file: %w", err)
	}

	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("cannot find executable: %w", err)
	}

	cmd := exec.Command(exe, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // Detach from terminal session
	}
	// Don't inherit stdin
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return 0, fmt.Errorf("cannot start daemon: %w", err)
	}
	logFile.Close()

	pid := cmd.Process.Pid

	// Write PID file
	pidPath, err := PIDFile()
	if err != nil {
		return pid, err
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return pid, fmt.Errorf("cannot write PID file: %w", err)
	}

	// Release the child process so it doesn't become a zombie
	_ = cmd.Process.Release()

	return pid, nil
}

// Stop sends SIGTERM to the daemon and waits for it to exit.
func Stop() error {
	pid, err := ReadPID()
	if err != nil {
		return err
	}
	if pid == 0 {
		return fmt.Errorf("no daemon PID file found")
	}
	if !IsRunning(pid) || !isOurDaemon(pid) {
		// Stale PID file, or the PID was recycled by an unrelated process —
		// either way we must NOT signal it. Clear the file and report stale.
		_ = RemovePID()
		return fmt.Errorf("daemon is not running (stale PID %d)", pid)
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("cannot find process %d: %w", pid, err)
	}

	// Send SIGTERM for graceful shutdown
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("cannot send SIGTERM to PID %d: %w", pid, err)
	}

	// Wait up to 10 seconds for the process to exit. Using timer/ticker
	// channels instead of time.Now() makes this immune to wall-clock
	// adjustments and avoids busy polling.
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if !IsRunning(pid) {
				_ = RemovePID()
				return nil
			}
		case <-timer.C:
			// Timeout reached: force kill.
			_ = proc.Signal(syscall.SIGKILL)
			_ = RemovePID()
			return nil
		}
	}
}
