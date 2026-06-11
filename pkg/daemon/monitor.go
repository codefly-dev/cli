// Process monitor for codefly — detects runaway agent processes.
// Runs in the background, checks CPU/memory of all codefly-related processes.
// Kills anything that exceeds thresholds and logs warnings.

package daemon

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// MonitorConfig configures the process monitor.
type MonitorConfig struct {
	CheckInterval time.Duration // how often to check (default 30s)
	CPUThreshold  float64       // kill if CPU% exceeds this for 2 checks (default 200%)
	MemoryMB      int           // warn if RSS exceeds this (default 512MB)
	MaxOrphans    int           // max orphaned agent processes (default 3)
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
	PID     int
	CPU     float64 // percentage
	MemRSS  int     // KB
	Command string
	Name    string // extracted: "go-grpc", "go-generic", "neo4j", etc.
}

// MonitorResult is the output of a single monitor check.
type MonitorResult struct {
	Timestamp  time.Time
	Processes  []ProcessInfo
	Warnings   []string
	Killed     []int // PIDs that were killed
	TotalCPU   float64
	TotalMemMB float64
}

// CheckProcesses scans for codefly-related processes and returns their status.
func CheckProcesses() ([]ProcessInfo, error) {
	// Use ps to get all processes with CPU and memory
	out, err := exec.Command("ps", "aux").Output()
	if err != nil {
		return nil, fmt.Errorf("ps failed: %w", err)
	}

	var processes []ProcessInfo
	patterns := []string{"go-grpc", "go-generic", "codefly", "neo4j", "mind-server"}

	for _, line := range strings.Split(string(out), "\n") {
		matched := false
		for _, pat := range patterns {
			if strings.Contains(line, pat) && !strings.Contains(line, "grep") {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}

		info := parsePSLine(line)
		if info != nil {
			processes = append(processes, *info)
		}
	}

	return processes, nil
}

// Monitor runs a single check and returns the result.
func Monitor(cfg MonitorConfig) (*MonitorResult, error) {
	procs, err := CheckProcesses()
	if err != nil {
		return nil, err
	}

	result := &MonitorResult{
		Timestamp: time.Now(),
		Processes: procs,
	}

	// Track orphaned agents (not part of any codefly session)
	orphanedAgents := 0

	for _, p := range procs {
		result.TotalCPU += p.CPU
		result.TotalMemMB += float64(p.MemRSS) / 1024

		// CPU threshold check
		if p.CPU > cfg.CPUThreshold {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("HIGH CPU: PID %d (%s) at %.1f%% CPU", p.PID, p.Name, p.CPU))

			// Kill runaway agent processes (not the main codefly process)
			if isAgentProcess(p.Name) {
				if err := killProcess(p.PID); err == nil {
					result.Killed = append(result.Killed, p.PID)
					result.Warnings = append(result.Warnings,
						fmt.Sprintf("KILLED: PID %d (%s) — CPU exceeded %.0f%%", p.PID, p.Name, cfg.CPUThreshold))
				}
			}
		}

		// Memory threshold check
		if p.MemRSS/1024 > cfg.MemoryMB {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("HIGH MEM: PID %d (%s) at %dMB", p.PID, p.Name, p.MemRSS/1024))
		}

		// Count orphaned agents
		if isAgentProcess(p.Name) {
			orphanedAgents++
		}
	}

	// Too many orphaned agents
	if orphanedAgents > cfg.MaxOrphans {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("ORPHANS: %d agent processes detected (max %d)", orphanedAgents, cfg.MaxOrphans))
	}

	return result, nil
}

// RunMonitorLoop runs the monitor continuously in the background.
// Call this as a goroutine from the codefly daemon.
func RunMonitorLoop(cfg MonitorConfig) {
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

	// Track consecutive high-CPU for the same PID (kill after 2 checks)
	highCPUCount := make(map[int]int)

	ticker := time.NewTicker(cfg.CheckInterval)
	defer ticker.Stop()

	for range ticker.C {
		result, err := Monitor(cfg)
		if err != nil {
			logger.Printf("check failed: %v", err)
			continue
		}

		// Update high-CPU tracking
		currentPIDs := make(map[int]bool)
		for _, p := range result.Processes {
			currentPIDs[p.PID] = true
			if p.CPU > cfg.CPUThreshold/2 { // track at half threshold
				highCPUCount[p.PID]++
				if highCPUCount[p.PID] >= 2 && isAgentProcess(p.Name) {
					logger.Printf("KILLING PID %d (%s): high CPU for %d consecutive checks (%.1f%%)",
						p.PID, p.Name, highCPUCount[p.PID], p.CPU)
					killProcess(p.PID)
				}
			} else {
				highCPUCount[p.PID] = 0
			}
		}

		// Clean up tracking for dead processes
		for pid := range highCPUCount {
			if !currentPIDs[pid] {
				delete(highCPUCount, pid)
			}
		}

		// Log warnings
		for _, w := range result.Warnings {
			logger.Println(w)
		}

		// Periodic status (every 10 checks)
		if len(result.Processes) > 0 {
			logger.Printf("status: %d processes, %.1f%% total CPU, %.0fMB total memory",
				len(result.Processes), result.TotalCPU, result.TotalMemMB)
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
	if len(fields) < 11 {
		return nil
	}

	pid, err := strconv.Atoi(fields[1])
	if err != nil {
		return nil
	}
	cpu, err := strconv.ParseFloat(fields[2], 64)
	if err != nil {
		return nil
	}
	// RSS is field 5 in ps aux
	rss, _ := strconv.Atoi(fields[5])

	command := strings.Join(fields[10:], " ")
	name := extractProcessName(command)

	return &ProcessInfo{
		PID:     pid,
		CPU:     cpu,
		MemRSS:  rss,
		Command: command,
		Name:    name,
	}
}

func extractProcessName(command string) string {
	for _, name := range []string{"go-grpc", "go-generic", "mind-server", "neo4j", "codefly"} {
		if strings.Contains(command, name) {
			return name
		}
	}
	return filepath.Base(strings.Fields(command)[0])
}

func isAgentProcess(name string) bool {
	return name == "go-grpc" || name == "go-generic"
}

func killProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(os.Kill)
}
