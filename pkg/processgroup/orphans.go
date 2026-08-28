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

// nativeBuildCacheSegment marks a compiled native-mode service binary. The go and
// rust runners build a service's user binary under "<service>/cache/native/<hash>"
// and exec it in place, so this segment in a process's executable identifies a
// codefly-built native service. (The issue's manual workaround greps the same
// path.)
const nativeBuildCacheSegment = "/cache/native/"

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
	// Owned is true when the process carries codefly's process-group
	// authentication in its environment, proving codefly spawned it. Only owned
	// groups are ever signalled; a dev server the user launched by hand inside a
	// workspace is surfaced by a scan but never reaped.
	Owned bool
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
		snap, ok := snapshotProcess(ctx, pid, proc)
		if !ok {
			continue
		}
		orphans = append(orphans, DevServerOrphan{
			PID:       pid,
			PGID:      snap.pgid,
			Parent:    snap.parent,
			Command:   strings.Join(argv, " "),
			Cwd:       cwd,
			Workspace: workspace,
			Started:   snap.started,
			Orphaned:  snap.orphaned,
			Owned:     snap.owned,
		})
	}
	return orphans, nil
}

// processSnapshot is the process-group, ownership, parentage, and timing view the
// orphan scanners share once a process has matched their signature. Building it in
// one place keeps the fiddly ordering (getpgid → ownership → parent → start time)
// from drifting between the dev-server and native-service scanners.
type processSnapshot struct {
	pgid     int
	parent   int
	orphaned bool
	owned    bool
	started  time.Time
}

// snapshotProcess gathers the shared view. ok is false when the process's group or
// ownership can no longer be read (it exited mid-scan, or its environment is
// unreadable) — the caller skips it, matching both scanners' original behavior.
func snapshotProcess(ctx context.Context, pid int, proc *process.Process) (processSnapshot, bool) {
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return processSnapshot{}, false
	}
	owned, err := processIsCodeflyOwned(pid)
	if err != nil {
		return processSnapshot{}, false
	}
	snap := processSnapshot{pgid: pgid, owned: owned}
	if ppid, err := proc.PpidWithContext(ctx); err == nil {
		snap.parent = int(ppid)
		snap.orphaned = ppid == 1
	}
	if started, err := proc.CreateTimeWithContext(ctx); err == nil {
		snap.started = time.UnixMilli(started)
	}
	return snap, true
}

// ReapDevServerOrphans terminates the process groups of leaked dev servers. A
// group is reaped only when it is (1) codefly-owned — a member carries codefly's
// process-group authentication, so codefly spawned it — and (2) stale — its
// group leader is gone or has itself been reparented to init. Dev servers the
// user launched by hand (no authentication) and servers still supervised by a
// live codefly (leader with a live parent) are left untouched. With dryRun set,
// nothing is signalled and the return value is what would be reaped.
func ReapDevServerOrphans(ctx context.Context, dryRun bool) ([]DevServerOrphan, error) {
	orphans, err := ScanDevServerOrphans(ctx)
	if err != nil {
		return nil, err
	}
	reapedGroups := make(map[int]struct{})
	var reaped []DevServerOrphan
	var failures []error
	for _, orphan := range orphans {
		if !orphan.Owned {
			continue
		}
		if _, done := reapedGroups[orphan.PGID]; done {
			continue
		}
		stale, err := groupIsStale(orphan.PGID)
		if err != nil {
			failures = append(failures, fmt.Errorf("inspect dev-server process group %d (%s): %w", orphan.PGID, orphan.Cwd, err))
			continue
		}
		if !stale {
			continue
		}
		reapedGroups[orphan.PGID] = struct{}{}
		reaped = append(reaped, orphan)
		if dryRun {
			continue
		}
		if err := killAuthenticatedProcessGroup(ctx, orphan.PGID); err != nil {
			failures = append(failures, fmt.Errorf("reap dev-server process group %d (%s): %w", orphan.PGID, orphan.Cwd, err))
		}
	}
	return reaped, errors.Join(failures...)
}

// NativeServiceOrphan is a native-mode service process found by signature rather
// than by registry record: a compiled user binary the go/rust runner built under a
// codefly build cache, or a PostgreSQL cluster codefly started for a host service.
// Native services bind deterministic per-workspace ports, so one that outlives its
// supervisor keeps LISTENing on that port and collides with the next `codefly run`
// — the "address already in use" boot failure or the stale backend still serving a
// module-federation manifest the issue describes. The registry reaper can no longer
// see these once its record is lost (a daemonized postmaster escapes its tracked
// group entirely); this signature scan is the fallback `codefly clear` needs.
//
// Unlike a dev server, a native service is not required to sit inside a workspace
// directory — a postgres data dir lives under ~/.codefly/data, outside the repo —
// so ownership rests entirely on the process-group authentication.
type NativeServiceOrphan struct {
	PID      int
	PGID     int
	Parent   int
	Command  string
	Cwd      string
	Started  time.Time
	Orphaned bool
	Owned    bool
}

// ScanNativeServiceOrphans finds native-mode service processes (a compiled user
// binary under a codefly build cache, or a postgres cluster) running anywhere on
// the machine, without relying on the process-group registry. Ownership is decided
// by the process-group authentication alone, so a user's own or system postgres is
// surfaced with Owned=false and never reaped.
func ScanNativeServiceOrphans(ctx context.Context) ([]NativeServiceOrphan, error) {
	pids, err := process.PidsWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("enumerate processes: %w", err)
	}
	self := os.Getpid()
	var orphans []NativeServiceOrphan
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
		if err != nil || !isNativeServiceCommand(argv) {
			continue
		}
		snap, ok := snapshotProcess(ctx, pid, proc)
		if !ok {
			continue
		}
		orphan := NativeServiceOrphan{
			PID:      pid,
			PGID:     snap.pgid,
			Parent:   snap.parent,
			Command:  strings.Join(argv, " "),
			Started:  snap.started,
			Orphaned: snap.orphaned,
			Owned:    snap.owned,
		}
		if cwd, err := processWorkingDirectory(pid); err == nil {
			orphan.Cwd = cwd
		}
		orphans = append(orphans, orphan)
	}
	return orphans, nil
}

// ReapNativeServiceOrphans terminates the process groups of leaked native-mode
// services. As with dev servers, a group is reaped only when it is (1) codefly-owned
// — a member carries the process-group authentication — and (2) stale — its leader
// is gone or reparented to init. With dryRun set, nothing is signalled and the
// return value is what would be reaped.
func ReapNativeServiceOrphans(ctx context.Context, dryRun bool) ([]NativeServiceOrphan, error) {
	orphans, err := ScanNativeServiceOrphans(ctx)
	if err != nil {
		return nil, err
	}
	reapedGroups := make(map[int]struct{})
	var reaped []NativeServiceOrphan
	var failures []error
	for _, orphan := range orphans {
		if !orphan.Owned {
			continue
		}
		if _, done := reapedGroups[orphan.PGID]; done {
			continue
		}
		stale, err := groupIsStale(orphan.PGID)
		if err != nil {
			failures = append(failures, fmt.Errorf("inspect native-service process group %d (%s): %w", orphan.PGID, orphan.Command, err))
			continue
		}
		if !stale {
			continue
		}
		reapedGroups[orphan.PGID] = struct{}{}
		reaped = append(reaped, orphan)
		if dryRun {
			continue
		}
		if err := killAuthenticatedProcessGroup(ctx, orphan.PGID); err != nil {
			failures = append(failures, fmt.Errorf("reap native-service process group %d (%s): %w", orphan.PGID, orphan.Command, err))
		}
	}
	return reaped, errors.Join(failures...)
}

// isNativeServiceCommand recognises the native-mode service process shapes that
// squat on deterministic ports after their supervisor dies: a compiled user binary
// the go/rust runner built under a codefly build cache, and the PostgreSQL
// postmaster codefly starts for a host service. Matching by the postgres name is
// deliberately broad; the authentication + staleness gates in
// ReapNativeServiceOrphans are what keep it from ever reaping a user's own or system
// postgres.
func isNativeServiceCommand(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	exe := argv[0]
	// Match the build-cache segment both mid-path (absolute binary, e.g.
	// "/ws/svc/code/cache/native/<hash>") and as a leading segment (a binary
	// built under a relative source dir, "cache/native/<hash>"). Anchoring on the
	// path separator is what keeps an unrelated "mycache/native/..." from matching.
	if strings.Contains(exe, nativeBuildCacheSegment) || strings.HasPrefix(exe, nativeBuildCacheSegment[1:]) {
		return true
	}
	return filepath.Base(exe) == "postgres"
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

// processIsCodeflyOwned reports whether pid carries codefly's process-group
// authentication in its environment — proof that codefly spawned it (and, via
// inheritance, its whole tree). This survives the loss of the registry record,
// so it identifies leaked codefly servers that `clear` may safely reap.
func processIsCodeflyOwned(pid int) (bool, error) {
	authentication, err := readProcessGroupAuthentication(pid)
	if errors.Is(err, errProcessNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return authentication != "", nil
}

// groupIsStale reports whether the process group has lost its supervisor. It is
// stale unless its leader (the process whose pid equals the pgid) is alive and
// still has a live parent — i.e. is being supervised. A gone or init-reparented
// leader means the group has escaped codefly and is safe to reap.
func groupIsStale(pgid int) (bool, error) {
	leader, err := inspectProcessIdentity(pgid)
	if errors.Is(err, errProcessNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if leader.pgid == pgid && leader.parent != 1 {
		return false, nil
	}
	return true, nil
}

// groupHasCodeflyMember reports whether the live process group still contains a
// codefly-owned member. It is re-checked immediately before each destructive
// signal so that a process group whose number was recycled between the scan and
// the kill is never signalled unless it is, right now, codefly's.
func groupHasCodeflyMember(ctx context.Context, pgid int) (bool, error) {
	members, err := inspectProcessGroup(ctx, pgid)
	if err != nil {
		return false, err
	}
	for _, member := range members {
		owned, err := processIsCodeflyOwned(member.pid)
		if err != nil {
			return false, err
		}
		if owned {
			return true, nil
		}
	}
	return false, nil
}

// killAuthenticatedProcessGroup escalates SIGTERM then SIGKILL to the group,
// but re-confirms the group still holds a codefly-owned member immediately
// before each signal. That closes the window between scan and kill in which the
// process-group number could have been recycled by an unrelated group: if the
// group is no longer codefly's (or already gone), it aborts without signalling.
func killAuthenticatedProcessGroup(ctx context.Context, pgid int) error {
	if pgid <= 1 {
		return fmt.Errorf("refusing to signal process group %d", pgid)
	}
	if pgid == syscall.Getpgrp() {
		return errors.New("refusing to signal own process group")
	}
	owned, err := groupHasCodeflyMember(ctx, pgid)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	if err = signalGroup(pgid, syscall.SIGTERM); err != nil {
		return err
	}
	if waitForGroupDeath(ctx, pgid, sigtermGrace) {
		return nil
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	owned, err = groupHasCodeflyMember(ctx, pgid)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	if err = signalGroup(pgid, syscall.SIGKILL); err != nil {
		return err
	}
	if waitForGroupDeath(ctx, pgid, sigkillGrace) {
		return nil
	}
	if err = ctx.Err(); err != nil {
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
