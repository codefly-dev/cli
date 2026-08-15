package processgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	runnersbase "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/wool"
	"github.com/gofrs/flock"
	"github.com/shirou/gopsutil/v3/process"
)

const (
	stateDirName        = "runs"
	registryLockName    = ".reaper.lock"
	maxSweepPasses      = 4
	recordReadAttempts  = 3
	recordReadRetry     = 10 * time.Millisecond
	dispositionFailed   = "failed"
	dispositionReaped   = "reaped"
	sigtermGrace        = 15 * time.Second
	sigkillGrace        = 2 * time.Second
	startTolerance      = 2 * time.Second
	createTimePrecision = time.Millisecond
)

var errLeaderExited = errors.New("process-group leader exited")

type record struct {
	pgid      int
	parent    int
	started   time.Time
	writtenAt time.Time
	command   string
}

type leaderIdentity struct {
	parent       int
	started      time.Time
	commandNames map[string]struct{}
}

var reapMu sync.Mutex

// ReapStaleProcessGroups reconciles both the current authenticated registry
// and the legacy root registry still written by independently released agents.
func ReapStaleProcessGroups(ctx context.Context) error {
	currentErr := runnersbase.ReapStaleProcessGroups(ctx)
	legacyErr := reapLegacyProcessGroups(ctx)
	return errors.Join(currentErr, legacyErr)
}

func reapLegacyProcessGroups(ctx context.Context) error {
	reapMu.Lock()
	defer reapMu.Unlock()

	dir, err := stateDir()
	if err != nil {
		return err
	}
	registryLock := flock.New(filepath.Join(dir, registryLockName))
	locked, err := registryLock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return fmt.Errorf("lock process-group registry: %w", err)
	}
	if !locked {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("lock process-group registry: %w", err)
		}
		return errors.New("lock process-group registry: lock was not acquired")
	}
	defer func() {
		_ = registryLock.Unlock()
		_ = registryLock.Close()
	}()

	var sweepErr error
	for range maxSweepPasses {
		reaped, passErr := sweep(ctx, dir)
		sweepErr = passErr
		if err := ctx.Err(); err != nil {
			return errors.Join(err, passErr)
		}
		if reaped == 0 {
			return passErr
		}
	}
	return sweepErr
}

func stateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine home directory: %w", err)
	}
	dir := filepath.Join(home, ".codefly", stateDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create process-group registry: %w", err)
	}
	return dir, nil
}

func sweep(ctx context.Context, dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read process-group registry: %w", err)
	}

	reaped := 0
	var failures []error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pgid") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		disposition, err := reconcile(ctx, path)
		if disposition == dispositionReaped {
			reaped++
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	return reaped, errors.Join(failures...)
}

func reconcile(ctx context.Context, path string) (string, error) {
	w := wool.Get(ctx).In("processgroup.reconcile")
	rec, err := readRecord(path)
	if err != nil {
		w.Warn("could not reconcile process-group record",
			wool.Field("record", path),
			wool.Field("disposition", "retained-invalid-record"),
			wool.ErrField(err))
		return dispositionFailed, fmt.Errorf("read process-group record %s: %w", path, err)
	}
	fields := []*wool.LogField{
		wool.Field("record", path),
		wool.Field("pgid", rec.pgid),
		wool.Field("parent", rec.parent),
	}

	if !groupAlive(rec.pgid) {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return "", fmt.Errorf("remove dead process-group record %s: %w", path, removeErr)
		}
		w.Debug("reconciled process-group record", append(fields,
			wool.Field("disposition", "removed-dead-group"))...)
		return "removed", nil
	}

	leader, err := inspectLeader(rec.pgid)
	if errors.Is(err, errLeaderExited) {
		return reconcileLeaderless(ctx, path, &rec, fields)
	}
	if err != nil {
		w.Warn("could not inspect process-group leader",
			append(fields,
				wool.Field("disposition", "retained-inspection-failure"),
				wool.ErrField(err))...)
		return dispositionFailed, fmt.Errorf("inspect process-group leader %d from record %s: %w", rec.pgid, path, err)
	}
	if leader.started.After(rec.writtenAt.Add(createTimePrecision)) {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return "", fmt.Errorf("remove rejected process-group record %s: %w", path, removeErr)
		}
		w.Warn("rejected process-group record for reused group without signaling group",
			append(fields,
				wool.Field("disposition", "rejected-reused-group"))...)
		return "rejected", nil
	}
	if !rec.matches(leader) {
		err = errors.New("recorded start time or command does not match the process-group leader")
		w.Warn("could not authenticate process-group record",
			append(fields,
				wool.Field("disposition", "retained-identity-mismatch"),
				wool.ErrField(err))...)
		return dispositionFailed, fmt.Errorf("authenticate process-group leader %d from record %s: %w", rec.pgid, path, err)
	}

	if leader.parent == rec.parent {
		ownerAlive, err := ownerPredatesRecord(&rec)
		if err != nil {
			w.Warn("could not inspect process-group owner",
				append(fields,
					wool.Field("disposition", "retained-owner-inspection-failure"),
					wool.ErrField(err))...)
			return dispositionFailed, fmt.Errorf("inspect process-group owner %d from record %s: %w", rec.parent, path, err)
		}
		if ownerAlive {
			w.Debug("reconciled process-group record", append(fields,
				wool.Field("disposition", "preserved-live-owner"))...)
			return "preserved", nil
		}
	}

	return reapGroup(ctx, path, &rec, fields)
}

func readRecord(path string) (record, error) {
	var lastErr error
	for attempt := range recordReadAttempts {
		rec, err := readRecordOnce(path)
		if err == nil {
			return rec, nil
		}
		lastErr = err
		if attempt == recordReadAttempts-1 {
			break
		}
		timer := time.NewTimer(recordReadRetry)
		<-timer.C
	}
	return record{}, lastErr
}

func readRecordOnce(path string) (record, error) {
	before, err := os.Stat(path)
	if err != nil {
		return record{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return record{}, err
	}
	after, err := os.Stat(path)
	if err != nil {
		return record{}, err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return record{}, errors.New("process-group record changed while being read")
	}
	values := make(map[string]string)
	for line := range strings.SplitSeq(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			values[key] = value
		}
	}
	pgidValue, err := strconv.ParseInt(values["pgid"], 10, 32)
	if err != nil || pgidValue <= 1 {
		return record{}, errors.New("invalid pgid")
	}
	pgid := int(pgidValue)
	parentValue, err := strconv.ParseInt(values["parent"], 10, 32)
	if err != nil || parentValue <= 1 {
		return record{}, errors.New("invalid parent")
	}
	parent := int(parentValue)
	startedUnix, err := strconv.ParseInt(values["started"], 10, 64)
	if err != nil || startedUnix <= 0 {
		return record{}, errors.New("invalid start time")
	}
	command := recordedCommand(values["cmd"])
	if command == "" {
		return record{}, errors.New("invalid command summary")
	}
	return record{
		pgid:      pgid,
		parent:    parent,
		started:   time.Unix(startedUnix, 0),
		writtenAt: after.ModTime(),
		command:   command,
	}, nil
}

func recordedCommand(summary string) string {
	separator := strings.LastIndex(summary, " <")
	if separator <= 0 || !strings.HasSuffix(summary, " args>") {
		return ""
	}
	return filepath.Base(summary[:separator])
}

func inspectLeader(pgid int) (leaderIdentity, error) {
	actualGroup, err := syscall.Getpgid(pgid)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return leaderIdentity{}, errLeaderExited
		}
		return leaderIdentity{}, err
	}
	if actualGroup != pgid {
		return leaderIdentity{}, fmt.Errorf("pid %d belongs to process group %d", pgid, actualGroup)
	}
	// #nosec G115 -- registry parsing rejects PGIDs outside the signed 32-bit process API.
	proc, err := process.NewProcess(int32(pgid))
	if err != nil {
		if errors.Is(err, process.ErrorProcessNotRunning) {
			return leaderIdentity{}, errLeaderExited
		}
		return leaderIdentity{}, err
	}
	startedMillis, err := proc.CreateTime()
	if err != nil {
		if errors.Is(err, process.ErrorProcessNotRunning) {
			return leaderIdentity{}, errLeaderExited
		}
		return leaderIdentity{}, fmt.Errorf("inspect leader start time: %w", err)
	}
	parent, err := proc.Ppid()
	if err != nil {
		return leaderIdentity{}, fmt.Errorf("inspect leader parent: %w", err)
	}
	names := make(map[string]struct{})
	if argv, argvErr := proc.CmdlineSlice(); argvErr == nil && len(argv) > 0 {
		names[filepath.Base(argv[0])] = struct{}{}
	}
	if executable, executableErr := proc.Exe(); executableErr == nil && executable != "" {
		names[filepath.Base(executable)] = struct{}{}
	}
	if name, nameErr := proc.Name(); nameErr == nil && name != "" {
		names[filepath.Base(name)] = struct{}{}
	}
	if len(names) == 0 {
		return leaderIdentity{}, errors.New("inspect leader command: no command identity available")
	}
	return leaderIdentity{
		parent:       int(parent),
		started:      time.UnixMilli(startedMillis),
		commandNames: names,
	}, nil
}

func reconcileLeaderless(ctx context.Context, path string, rec *record, fields []*wool.LogField) (string, error) {
	w := wool.Get(ctx).In("processgroup.reconcile")
	authenticated, err := groupPredatesRecord(rec)
	if err != nil {
		w.Warn("could not authenticate leaderless process group",
			append(fields,
				wool.Field("disposition", "retained-leaderless-inspection-failure"),
				wool.ErrField(err))...)
		return dispositionFailed, fmt.Errorf("authenticate leaderless process group %d from record %s: %w", rec.pgid, path, err)
	}
	if !authenticated {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return "", fmt.Errorf("remove rejected process-group record %s: %w", path, removeErr)
		}
		w.Warn("rejected process-group record for reused leaderless group without signaling group",
			append(fields, wool.Field("disposition", "rejected-reused-leaderless-group"))...)
		return "rejected", nil
	}
	ownerAlive, err := ownerPredatesRecord(rec)
	if err != nil {
		w.Warn("could not inspect process-group owner",
			append(fields,
				wool.Field("disposition", "retained-owner-inspection-failure"),
				wool.ErrField(err))...)
		return dispositionFailed, fmt.Errorf("inspect process-group owner %d from record %s: %w", rec.parent, path, err)
	}
	if ownerAlive {
		w.Debug("reconciled process-group record", append(fields,
			wool.Field("disposition", "preserved-live-owner-leaderless-group"))...)
		return "preserved", nil
	}
	return reapGroup(ctx, path, rec, fields)
}

func groupPredatesRecord(rec *record) (bool, error) {
	pids, err := process.Pids()
	if err != nil {
		return false, err
	}
	found := false
	for _, pid := range pids {
		actualGroup, err := syscall.Getpgid(int(pid))
		if errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("inspect process %d group: %w", pid, err)
		}
		if actualGroup != rec.pgid {
			continue
		}
		found = true
		member, err := process.NewProcess(pid)
		if err != nil {
			if errors.Is(err, process.ErrorProcessNotRunning) {
				continue
			}
			return false, fmt.Errorf("inspect process-group member %d: %w", pid, err)
		}
		startedMillis, err := member.CreateTime()
		if err != nil {
			if errors.Is(err, process.ErrorProcessNotRunning) {
				continue
			}
			return false, fmt.Errorf("inspect process-group member %d start time: %w", pid, err)
		}
		if !time.UnixMilli(startedMillis).After(rec.writtenAt.Add(createTimePrecision)) {
			return true, nil
		}
	}
	if !found && groupAlive(rec.pgid) {
		return false, errors.New("live process group had no inspectable members")
	}
	return false, nil
}

func ownerPredatesRecord(rec *record) (bool, error) {
	// #nosec G115 -- registry parsing rejects parent PIDs outside the signed 32-bit process API.
	alive, err := process.PidExists(int32(rec.parent))
	if err != nil {
		return false, err
	}
	if !alive {
		return false, nil
	}
	// #nosec G115 -- registry parsing rejects parent PIDs outside the signed 32-bit process API.
	owner, err := process.NewProcess(int32(rec.parent))
	if err != nil {
		if errors.Is(err, process.ErrorProcessNotRunning) {
			return false, nil
		}
		return false, err
	}
	startedMillis, err := owner.CreateTime()
	if err != nil {
		if errors.Is(err, process.ErrorProcessNotRunning) {
			return false, nil
		}
		return false, err
	}
	return !time.UnixMilli(startedMillis).After(rec.writtenAt.Add(createTimePrecision)), nil
}

func reapGroup(ctx context.Context, path string, rec *record, fields []*wool.LogField) (string, error) {
	w := wool.Get(ctx).In("processgroup.reconcile")
	w.Warn("reaping stale managed process group",
		append(fields, wool.Field("disposition", "terminating-stale-owner"))...)
	if err := terminateGroup(ctx, rec.pgid); err != nil {
		w.Warn("failed to reap stale managed process group",
			append(fields,
				wool.Field("disposition", "reap-failed"),
				wool.ErrField(err))...)
		return dispositionFailed, fmt.Errorf("reap process group %d from record %s: %w", rec.pgid, path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return dispositionReaped, fmt.Errorf("remove reaped process-group record %s: %w", path, err)
	}
	w.Info("reconciled stale managed process group",
		append(fields, wool.Field("disposition", dispositionReaped))...)
	return dispositionReaped, nil
}

func (rec *record) matches(leader leaderIdentity) bool {
	if difference := rec.started.Sub(leader.started); difference < -startTolerance || difference > startTolerance {
		return false
	}
	_, ok := leader.commandNames[rec.command]
	return ok
}

func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func terminateGroup(ctx context.Context, pgid int) error {
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	if waitForGroupDeath(ctx, pgid, sigtermGrace) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	if waitForGroupDeath(ctx, pgid, sigkillGrace) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("process group remained alive after SIGKILL")
}

func waitForGroupDeath(ctx context.Context, pgid int, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !groupAlive(pgid) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return !groupAlive(pgid)
		case <-ticker.C:
		}
	}
}
