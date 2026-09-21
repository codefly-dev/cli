// Process monitor for codefly — observes agent process resource usage.
// Runs in the background, checks CPU/memory of codefly-related processes, and
// logs warnings. Process termination belongs to the session-aware reaper, which
// knows the process group and parentage; this monitor deliberately never kills.

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/codefly-dev/core/runners/base"
)

// MonitorConfig configures the process monitor.
type MonitorConfig struct {
	CheckInterval time.Duration // how often to check (default 30s)
	CPUThreshold  float64       // warn if CPU% exceeds this (default 200%)
	MemoryMB      int           // warn if RSS exceeds this (default 512MB)
	MaxOrphans    int           // max authenticated groups whose owner exited (default 3)
	LogPath       string        // monitor log file
}

// DefaultMonitorConfig returns sensible defaults.
func DefaultMonitorConfig() MonitorConfig {
	logDir, _ := stateDir()
	return MonitorConfig{
		CheckInterval: 30 * time.Second,
		CPUThreshold:  200.0,
		MemoryMB:      512,
		MaxOrphans:    3,
		LogPath:       filepath.Join(logDir, "monitor.log"),
	}
}

// ProcessInfo describes a running codefly-related process.
type ProcessInfo struct {
	PID        int
	PGID       int
	OwnerAlive bool
	CPU        float64 // percentage
	MemRSS     int     // KB
	Command    string
	Name       string // executable reported by authenticated process inspection
}

// MonitorResult is the output of a single monitor check.
type MonitorResult struct {
	Timestamp  time.Time
	Processes  []ProcessInfo
	Warnings   []string
	Killed     []int // Deprecated: retained for compatibility; always empty.
	TotalCPU   float64
	TotalMemMB float64
}

// CheckProcesses scans for codefly-related processes and returns their status.
func CheckProcesses() ([]ProcessInfo, error) {
	return checkProcesses(context.Background())
}

func checkProcesses(ctx context.Context) ([]ProcessInfo, error) {
	// Do not collect command arguments: they may contain secrets and prove no
	// ownership. Core authenticates registered group members, including children
	// whose leader exited; executable names are display-only.
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,pgid=,pcpu=,rss=,comm=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps failed: %w", err)
	}

	var processes []ProcessInfo
	groups := make(map[int]*base.ProcessGroupOwnership)
	for _, line := range strings.Split(string(out), "\n") {
		info := parsePSLine(line)
		if info == nil {
			continue
		}
		ownership, inspected := groups[info.PGID]
		if !inspected {
			group, lookupErr := base.LookupProcessGroup(info.PGID)
			if errors.Is(lookupErr, base.ErrProcessGroupNotRegistered) {
				groups[info.PGID] = nil
				continue
			}
			if lookupErr != nil {
				return nil, lookupErr
			}
			var inspectErr error
			ownership, inspectErr = group.InspectOwnership(ctx)
			if inspectErr != nil {
				return nil, inspectErr
			}
			groups[info.PGID] = ownership
		}
		if ownership == nil || !ownership.Authenticated {
			continue
		}
		for _, member := range ownership.Members {
			if member.PID != info.PID {
				continue
			}
			info.OwnerAlive = ownership.OwnerAlive
			info.Command = member.Executable
			info.Name = filepath.Base(member.Executable)
			processes = append(processes, *info)
			break
		}
	}

	return processes, nil
}

// Monitor runs a single check and returns the result.
func Monitor(cfg MonitorConfig) (*MonitorResult, error) {
	return monitor(context.Background(), cfg)
}

func monitor(ctx context.Context, cfg MonitorConfig) (*MonitorResult, error) {
	procs, err := checkProcesses(ctx)
	if err != nil {
		return nil, err
	}

	result := &MonitorResult{
		Timestamp: time.Now(),
		Processes: procs,
	}

	orphanedGroups := make(map[int]bool)

	for _, p := range procs {
		result.TotalCPU += p.CPU
		result.TotalMemMB += float64(p.MemRSS) / 1024

		// CPU threshold check
		if p.CPU > cfg.CPUThreshold {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("HIGH CPU: PID %d (%s) at %.1f%% CPU", p.PID, p.Name, p.CPU))

		}

		// Memory threshold check
		if p.MemRSS/1024 > cfg.MemoryMB {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("HIGH MEM: PID %d (%s) at %dMB", p.PID, p.Name, p.MemRSS/1024))
		}

		if !p.OwnerAlive {
			orphanedGroups[p.PGID] = true
		}
	}

	// Too many orphaned agents
	if len(orphanedGroups) > cfg.MaxOrphans {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("ORPHANS: %d authenticated process groups have exited owners (max %d)", len(orphanedGroups), cfg.MaxOrphans))
	}

	return result, nil
}

// RunMonitorLoop runs the monitor continuously in the background.
// Call this as a goroutine from the codefly daemon.
func RunMonitorLoop(ctx context.Context, cfg MonitorConfig) error {
	var logger *log.Logger
	if cfg.LogPath != "" {
		f, err := os.OpenFile(cfg.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			defer f.Close()
			logger = log.New(f, "[monitor] ", log.LstdFlags)
		}
	}
	if logger == nil {
		logger = log.New(os.Stderr, "[monitor] ", log.LstdFlags)
	}

	ticker := time.NewTicker(cfg.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			result, err := monitor(ctx, cfg)
			if err != nil {
				logger.Printf("check failed: %v", err)
				continue
			}

			// Log warnings
			for _, w := range result.Warnings {
				logger.Println(w)
			}

			if len(result.Processes) > 0 {
				logger.Printf("status: %d processes, %.1f%% total CPU, %.0fMB total memory",
					len(result.Processes), result.TotalCPU, result.TotalMemMB)
			}
		}
	}
}

// FormatStatus returns a human-readable status string.
func FormatStatus(result *MonitorResult) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Codefly Process Monitor — %s\n", result.Timestamp.Format("15:04:05")))
	sb.WriteString(fmt.Sprintf("Total: %d processes, %.1f%% CPU, %.0fMB memory\n\n",
		len(result.Processes), result.TotalCPU, result.TotalMemMB))

	if len(result.Processes) == 0 {
		sb.WriteString("No codefly processes running.\n")
		return sb.String()
	}

	sb.WriteString(fmt.Sprintf("%-8s %-6s %-8s %s\n", "PID", "CPU%", "MEM(MB)", "PROCESS"))
	sb.WriteString(strings.Repeat("─", 50) + "\n")
	for _, p := range result.Processes {
		sb.WriteString(fmt.Sprintf("%-8d %-6.1f %-8d %s\n",
			p.PID, p.CPU, p.MemRSS/1024, p.Name))
	}

	if len(result.Warnings) > 0 {
		sb.WriteString("\nWarnings:\n")
		for _, w := range result.Warnings {
			sb.WriteString("  ⚠ " + w + "\n")
		}
	}

	return sb.String()
}

// ── Helpers ───────────────────────────────────────────────

func parsePSLine(line string) *ProcessInfo {
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return nil
	}

	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return nil
	}
	pgid, err := strconv.Atoi(fields[1])
	if err != nil || pgid <= 0 {
		return nil
	}
	cpu, err := strconv.ParseFloat(fields[2], 64)
	if err != nil {
		return nil
	}
	rss, err := strconv.Atoi(fields[3])
	if err != nil {
		return nil
	}

	command := strings.Join(fields[4:], " ")

	return &ProcessInfo{
		PID:     pid,
		PGID:    pgid,
		CPU:     cpu,
		MemRSS:  rss,
		Command: command,
		Name:    filepath.Base(command),
	}
}
