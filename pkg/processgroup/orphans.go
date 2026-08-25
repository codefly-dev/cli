package processgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/shirou/gopsutil/v3/process"
)

// maxWorkspaceWalk bounds how far up from a process's working directory we look
// for a workspace marker before giving up.
const maxWorkspaceWalk = 64

// DevServerOrphan is a frontend dev server found by signature rather than by
// registry record: a dev-server-shaped process whose working directory sits
// inside a codefly workspace. These leak when the `codefly run` / daemon that
// launched them exits and the process-group registry loses the record, leaving
// the server reparented to the init process and spinning a CPU core unnoticed.
type DevServerOrphan struct {
	PID       int
	PGID      int
	Parent    int
	Command   string
	Cwd       string
	Workspace string
	Started   time.Time
	// Orphaned is true when the process has been reparented away from its
	// codefly supervisor (PPID 1) — the escaped, no-longer-tracked leak that
	// the registry-based reaper can no longer identify.
	Orphaned bool
}

// ScanDevServerOrphans finds frontend dev servers running inside a codefly
// workspace, machine-wide, without relying on the process-group registry. It
// is the fallback discovery path for servers that escaped tracking. A process
// qualifies only when its working directory is enclosed by a codefly workspace,
// so unrelated dev servers the user runs outside codefly are never matched.
func ScanDevServerOrphans(ctx context.Context) ([]DevServerOrphan, error) {
	pids, err := process.PidsWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("enumerate processes: %w", err)
	}
	self := os.Getpid()
	var orphans []DevServerOrphan
	for _, rawPID := range pids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid := int(rawPID)
		if pid <= 1 || pid == self {
			continue
		}
		proc, err := process.NewProcessWithContext(ctx, rawPID)
		if err != nil {
			continue
		}
		argv, err := proc.CmdlineSliceWithContext(ctx)
		if err != nil || !isDevServerCommand(argv) {
			continue
		}
		cwd, err := processWorkingDirectory(pid)
		if err != nil || cwd == "" {
			continue
		}
		workspace, ok := enclosingWorkspace(cwd)
		if !ok {
			continue
		}
		pgid, err := syscall.Getpgid(pid)
		if err != nil {
			continue
		}
		orphan := DevServerOrphan{
			PID:       pid,
			PGID:      pgid,
			Command:   strings.Join(argv, " "),
			Cwd:       cwd,
			Workspace: workspace,
		}
		if ppid, err := proc.PpidWithContext(ctx); err == nil {
			orphan.Parent = int(ppid)
			orphan.Orphaned = ppid == 1
		}
		if started, err := proc.CreateTimeWithContext(ctx); err == nil {
			orphan.Started = time.UnixMilli(started)
		}
		orphans = append(orphans, orphan)
	}
	return orphans, nil
}

// ReapDevServerOrphans terminates the process groups of orphaned dev servers
// (those reparented away from their codefly supervisor). Servers still owned by
// a live supervisor are left alone — the registry-based reaper handles those.
// It returns one entry per reaped process group; with dryRun set, nothing is
// signalled and the return value is what would be reaped.
func ReapDevServerOrphans(ctx context.Context, dryRun bool) ([]DevServerOrphan, error) {
	orphans, err := ScanDevServerOrphans(ctx)
	if err != nil {
		return nil, err
	}
	reapedGroups := make(map[int]struct{})
	var reaped []DevServerOrphan
	var failures []error
	for _, orphan := range orphans {
		if !orphan.Orphaned {
			continue
		}
		if _, done := reapedGroups[orphan.PGID]; done {
			continue
		}
		reapedGroups[orphan.PGID] = struct{}{}
		reaped = append(reaped, orphan)
		if dryRun {
			continue
		}
		if err := killProcessGroup(ctx, orphan.PGID); err != nil {
			failures = append(failures, fmt.Errorf("reap dev-server process group %d (%s): %w", orphan.PGID, orphan.Cwd, err))
		}
	}
	return reaped, errors.Join(failures...)
}

// isDevServerCommand recognises the frontend dev-server process shapes that
// leak: `next dev`, the long-lived `next-server` child it execs into, a
// package-manager `run dev` script, and `vite`. The enclosing-workspace check
// in ScanDevServerOrphans is what keeps this from matching unrelated servers,
// so this stays intentionally broad.
func isDevServerCommand(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	for _, arg := range argv {
		switch filepath.Base(arg) {
		case "next-server", "vite":
			return true
		}
	}
	joined := strings.Join(argv, " ")
	return strings.Contains(joined, "next dev") || strings.Contains(joined, "run dev")
}

// enclosingWorkspace returns the nearest ancestor of dir (inclusive) that holds
// a workspace configuration file, marking dir as part of a codefly workspace.
func enclosingWorkspace(dir string) (string, bool) {
	for range maxWorkspaceWalk {
		if isRegularFile(filepath.Join(dir, resources.WorkspaceConfigurationName)) {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
	return "", false
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func killProcessGroup(ctx context.Context, pgid int) error {
	if pgid <= 1 {
		return fmt.Errorf("refusing to signal process group %d", pgid)
	}
	if pgid == syscall.Getpgrp() {
		return errors.New("refusing to signal own process group")
	}
	if err := signalGroup(pgid, syscall.SIGTERM); err != nil {
		return err
	}
	if waitForGroupDeath(ctx, pgid, sigtermGrace) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := signalGroup(pgid, syscall.SIGKILL); err != nil {
		return err
	}
	if waitForGroupDeath(ctx, pgid, sigkillGrace) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("process group %d remained alive after SIGKILL", pgid)
}

func signalGroup(pgid int, signal syscall.Signal) error {
	if err := syscall.Kill(-pgid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal process group %d: %w", pgid, err)
	}
	return nil
}
