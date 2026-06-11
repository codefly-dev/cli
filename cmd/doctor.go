package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/daemon"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/tui"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
)

// DoctorCmd runs a set of environment health checks — the things that silently
// break a `codefly run` and leave the user staring at a cryptic hang or error.
// Each check reports OK / WARN / FAIL with a concrete fix, so "it doesn't work"
// becomes a checklist. Exits non-zero if any hard check FAILs.
var DoctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check your environment for common codefly problems",
	Long: `Run environment health checks and print actionable fixes.

Verifies the things that quietly break a run — Docker reachability, the codefly
home + installed agents, free disk, process limits (macOS), and stray agent
processes — and tells you how to fix each one.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
		defer cancel()

		checks := []func(context.Context) checkResult{
			checkCodeflyHome,
			checkAgentsInstalled,
			checkDocker,
			checkDiskSpace,
			checkProcLimit,
			checkDaemon,
			checkStrayAgents,
			checkStaleSockets,
		}

		anyFail := false
		fmt.Println(tui.RenderHeader(1, "codefly doctor"))
		for _, c := range checks {
			r := c(ctx)
			r.print()
			if r.status == statusFail {
				anyFail = true
			}
		}

		fmt.Println()
		if anyFail {
			fmt.Println(tui.RenderError("Some checks FAILED — fix the items marked ✗ above."))
			cli.ExitError()
		}
		fmt.Println(tui.RenderInfo("All critical checks passed."))
		return nil
	},
}

type checkStatus int

const (
	statusOK checkStatus = iota
	statusWarn
	statusFail
)

type checkResult struct {
	name   string
	status checkStatus
	detail string
	fix    string // shown on WARN/FAIL
}

func (r checkResult) print() {
	// RenderWarning/RenderError already prepend their own ⚠️/☠️ glyphs, so only
	// the OK line needs an explicit marker.
	switch r.status {
	case statusOK:
		fmt.Println(tui.RenderInfo(fmt.Sprintf("✓ %s — %s", r.name, r.detail)))
	case statusWarn:
		fmt.Println(tui.RenderWarning(fmt.Sprintf("%s — %s", r.name, r.detail)))
		if r.fix != "" {
			fmt.Println(tui.RenderErrorDetail("    fix: " + r.fix))
		}
	case statusFail:
		fmt.Println(tui.RenderError(fmt.Sprintf("%s — %s", r.name, r.detail)))
		if r.fix != "" {
			fmt.Println(tui.RenderErrorDetail("    fix: " + r.fix))
		}
	}
}

func checkCodeflyHome(_ context.Context) checkResult {
	home := resources.CodeflyHomeDir()
	r := checkResult{name: "codefly home"}
	if err := os.MkdirAll(home, 0o755); err != nil {
		r.status = statusFail
		r.detail = fmt.Sprintf("%s is not writable: %v", home, err)
		r.fix = fmt.Sprintf("ensure %s exists and is writable (or set CODEFLY_HOME)", home)
		return r
	}
	// Confirm writability with a temp file.
	probe := filepath.Join(home, ".doctor-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		r.status = statusFail
		r.detail = fmt.Sprintf("%s is not writable: %v", home, err)
		r.fix = "check permissions on " + home
		return r
	}
	_ = os.Remove(probe)
	r.status = statusOK
	r.detail = home
	return r
}

func checkAgentsInstalled(_ context.Context) checkResult {
	home := resources.CodeflyHomeDir()
	agentsDir := filepath.Join(home, "agents")
	r := checkResult{name: "agents installed"}
	count := 0
	_ = filepath.WalkDir(agentsDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "agent.codefly.yaml") {
			count++
		}
		return nil
	})
	if count == 0 {
		r.status = statusWarn
		r.detail = "no agents found under " + agentsDir
		r.fix = "build/install agents (e.g. `cd agents/services/go-grpc && codefly agent build`)"
		return r
	}
	r.status = statusOK
	r.detail = fmt.Sprintf("%d agent(s) under %s", count, agentsDir)
	return r
}

func checkDocker(ctx context.Context) checkResult {
	r := checkResult{name: "docker daemon"}
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		r.status = statusWarn
		r.detail = "cannot create docker client: " + err.Error()
		r.fix = "install Docker if you plan to run docker-backed services (postgres, neo4j, …)"
		return r
	}
	defer c.Close()
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := c.Ping(pingCtx); err != nil {
		r.status = statusWarn // WARN, not FAIL: docker is optional for non-docker services
		r.detail = fmt.Sprintf("not reachable at %s (%v)", c.DaemonHost(), err)
		r.fix = "start Docker Desktop / dockerd, then `docker info` to confirm (only needed for docker-backed services)"
		return r
	}
	r.status = statusOK
	r.detail = "reachable at " + c.DaemonHost()
	return r
}

func checkDiskSpace(_ context.Context) checkResult {
	home := resources.CodeflyHomeDir()
	r := checkResult{name: "disk space"}
	var st syscall.Statfs_t
	if err := syscall.Statfs(home, &st); err != nil {
		r.status = statusWarn
		r.detail = "cannot stat filesystem: " + err.Error()
		return r
	}
	freeBytes := st.Bavail * uint64(st.Bsize)
	freeGB := float64(freeBytes) / (1024 * 1024 * 1024)
	switch {
	case freeGB < 2:
		r.status = statusFail
		r.detail = fmt.Sprintf("only %.1f GB free on %s", freeGB, home)
		r.fix = "free disk space; run `codefly clear` to remove stale containers/data dirs"
	case freeGB < 10:
		r.status = statusWarn
		r.detail = fmt.Sprintf("%.1f GB free on %s", freeGB, home)
		r.fix = "docker images + service data dirs can grow fast; keep an eye on it"
	default:
		r.status = statusOK
		r.detail = fmt.Sprintf("%.0f GB free", freeGB)
	}
	return r
}

// checkProcLimit verifies macOS kern.maxproc is high enough for the codefly
// stack (many agents + child processes). The default 2500 is too low.
func checkProcLimit(ctx context.Context) checkResult {
	r := checkResult{name: "process limit"}
	if runtime.GOOS != "darwin" {
		r.status = statusOK
		r.detail = "n/a (" + runtime.GOOS + ")"
		return r
	}
	const recommended = 6144
	out, err := exec.CommandContext(ctx, "sysctl", "-n", "kern.maxproc").Output()
	if err != nil {
		r.status = statusWarn
		r.detail = "cannot read kern.maxproc: " + err.Error()
		return r
	}
	maxproc, convErr := strconv.Atoi(strings.TrimSpace(string(out)))
	if convErr != nil {
		r.status = statusWarn
		r.detail = "unexpected kern.maxproc value: " + strings.TrimSpace(string(out))
		return r
	}
	if maxproc < recommended {
		r.status = statusWarn
		r.detail = fmt.Sprintf("kern.maxproc=%d (recommended ≥ %d)", maxproc, recommended)
		r.fix = fmt.Sprintf("sudo sysctl -w kern.maxproc=%d (and kern.maxprocperuid); persist in /etc/sysctl.conf", recommended)
		return r
	}
	r.status = statusOK
	r.detail = fmt.Sprintf("kern.maxproc=%d", maxproc)
	return r
}

func checkDaemon(_ context.Context) checkResult {
	r := checkResult{name: "daemon"}
	st, err := daemon.GetStatus()
	if err != nil || st == nil {
		r.status = statusOK
		r.detail = "not running (started on demand)"
		return r
	}
	if st.Running {
		r.status = statusOK
		r.detail = fmt.Sprintf("running (PID %d)", st.PID)
		return r
	}
	r.status = statusOK
	r.detail = "not running (started on demand)"
	return r
}

// checkStrayAgents reports codefly agent processes currently running. Advisory:
// a leftover count after everything should be stopped points at a leak the user
// can clean with `codefly clear`.
func checkStrayAgents(ctx context.Context) checkResult {
	r := checkResult{name: "stray agent processes"}
	out, err := exec.CommandContext(ctx, "ps", "-axo", "command=").Output()
	if err != nil {
		r.status = statusOK
		r.detail = "could not enumerate processes"
		return r
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "/.codefly/agents/") {
			count++
		}
	}
	if count > 0 {
		r.status = statusWarn
		r.detail = fmt.Sprintf("%d agent process(es) currently running", count)
		r.fix = "if no `codefly run` is active, run `codefly clear` to reap them"
		return r
	}
	r.status = statusOK
	r.detail = "none running"
	return r
}

// checkStaleSockets reports per-spawn UDS sockets left behind by crashed CLIs.
// Advisory — `codefly clear` (or the next agent load) reaps them.
func checkStaleSockets(_ context.Context) checkResult {
	r := checkResult{name: "stale sockets"}
	n := manager.CountStaleAgentSockets()
	if n > 0 {
		r.status = statusWarn
		r.detail = fmt.Sprintf("%d orphaned agent socket(s) under %s", n, filepath.Join(os.TempDir(), "codefly-uds"))
		r.fix = "run `codefly clear` to reap them (also reaped automatically on the next run)"
		return r
	}
	r.status = statusOK
	r.detail = "none"
	return r
}

func init() {
	RootCmd.AddCommand(DoctorCmd)
}
