package processgroup

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	runnersbase "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/wool"
	"github.com/gofrs/flock"
	"github.com/shirou/gopsutil/v3/process"
)

const (
	stateDirName         = "runs"
	registryLockName     = ".reaper.lock"
	maxSweepPasses       = 4
	maxRecordSize        = 16 << 10
	recordReadAttempts   = 3
	recordReadRetry      = 10 * time.Millisecond
	groupAuthBytes       = 32
	groupAuthEnv         = "CODEFLY_PROCESS_GROUP_AUTH"
	dispositionFailed    = "failed"
	dispositionReaped    = "reaped"
	dispositionRemoved   = "removed"
	dispositionRejected  = "rejected"
	dispositionPreserved = "preserved"
	sigtermGrace         = 15 * time.Second
	sigkillGrace         = 2 * time.Second
	startTolerance       = 2 * time.Second
	createTimePrecision  = time.Millisecond
)

var errLeaderExited = errors.New("process-group leader exited")

var errProcessGroupIdentityChanged = errors.New("process group identity changed")

type recordContract uint8

const (
	legacyLineRecord recordContract = iota
	authenticatedJSONRecord
)

type recordedProcessIdentity struct {
	PID        int    `json:"pid"`
	BootID     string `json:"boot_id"`
	StartID    uint64 `json:"start_id"`
	Executable string `json:"executable"`
}

type authenticatedRecord struct {
	PGID           int                     `json:"pgid"`
	Leader         recordedProcessIdentity `json:"leader"`
	Owner          recordedProcessIdentity `json:"owner"`
	Authentication string                  `json:"authentication"`
}

type recordSnapshot struct {
	info os.FileInfo
}

type record struct {
	contract  recordContract
	pgid      int
	parent    int
	started   time.Time
	writtenAt time.Time
	command   string
	auth      authenticatedRecord
}

type leaderIdentity struct {
	parent       int
	started      time.Time
	commandNames map[string]struct{}
}

type processIdentity struct {
	pid          int
	pgid         int
	parent       int
	started      time.Time
	bootID       string
	startID      uint64
	executable   string
	commandNames map[string]struct{}
}

type nativeProcessIdentity struct {
	pid        int
	pgid       int
	parent     int
	bootID     string
	startID    uint64
	executable string
}

type processSignalHandle interface {
	Signal(syscall.Signal) error
	Close() error
}

var legacyRegistryProcessLock = make(chan struct{}, 1)

// ReapStaleProcessGroups reconciles both the current authenticated registry
// and the legacy root registry still written by independently released agents.
func ReapStaleProcessGroups(ctx context.Context) error {
	currentErr := runnersbase.ReapStaleProcessGroups(ctx)
	legacyErr := reapLegacyProcessGroups(ctx)
	return errors.Join(currentErr, legacyErr)
}

func reapLegacyProcessGroups(ctx context.Context) error {
	select {
	case legacyRegistryProcessLock <- struct{}{}:
		defer func() { <-legacyRegistryProcessLock }()
	case <-ctx.Done():
		return ctx.Err()
	}

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

	var failures []error
	for range maxSweepPasses {
		reaped, passErr := sweep(ctx, dir)
		if passErr != nil {
			failures = append(failures, passErr)
		}
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		if reaped == 0 {
			break
		}
	}
	return errors.Join(failures...)
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
	rec, snapshot, err := readRecord(ctx, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return dispositionRemoved, nil
		}
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
		if removeErr := removeRecord(path, snapshot); removeErr != nil {
			return "", fmt.Errorf("remove dead process-group record %s: %w", path, removeErr)
		}
		w.Debug("reconciled process-group record", append(fields,
			wool.Field("disposition", "removed-dead-group"))...)
		return dispositionRemoved, nil
	}
	if rec.contract == authenticatedJSONRecord {
		return reconcileAuthenticated(ctx, path, &rec, snapshot, fields)
	}

	leader, err := inspectLeader(rec.pgid)
	if errors.Is(err, errLeaderExited) {
		return reconcileLeaderless(ctx, path, &rec, snapshot, fields)
	}
	if err != nil {
		w.Warn("could not inspect process-group leader",
			append(fields,
				wool.Field("disposition", "retained-inspection-failure"),
				wool.ErrField(err))...)
		return dispositionFailed, fmt.Errorf("inspect process-group leader %d from record %s: %w", rec.pgid, path, err)
	}
	if leader.started.After(rec.writtenAt.Add(createTimePrecision)) {
		if removeErr := removeRecord(path, snapshot); removeErr != nil {
			return "", fmt.Errorf("remove rejected process-group record %s: %w", path, removeErr)
		}
		w.Warn("rejected process-group record for reused group without signaling group",
			append(fields,
				wool.Field("disposition", "rejected-reused-group"))...)
		return dispositionRejected, nil
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
			return dispositionPreserved, nil
		}
	}

	return reapGroup(ctx, path, &rec, snapshot, fields)
}

func reconcileAuthenticated(ctx context.Context, path string, rec *record, snapshot recordSnapshot, fields []*wool.LogField) (string, error) {
	w := wool.Get(ctx).In("processgroup.reconcile")
	_, authenticated, err := authenticateAuthenticatedGroup(ctx, &rec.auth)
	if err != nil {
		return dispositionFailed, fmt.Errorf("authenticate process group %d from record %s: %w", rec.pgid, path, err)
	}
	if !authenticated {
		if removeErr := removeRecord(path, snapshot); removeErr != nil {
			return "", fmt.Errorf("remove rejected process-group record %s: %w", path, removeErr)
		}
		w.Warn("rejected authenticated process-group record without signaling group",
			append(fields, wool.Field("disposition", "rejected-reused-group"))...)
		return dispositionRejected, nil
	}
	ownerAlive, err := recordedOwnerAlive(rec.auth.Owner)
	if err != nil {
		return dispositionFailed, fmt.Errorf("inspect process-group owner %d from record %s: %w", rec.auth.Owner.PID, path, err)
	}
	if ownerAlive {
		w.Debug("reconciled process-group record", append(fields,
			wool.Field("disposition", "preserved-live-owner"))...)
		return dispositionPreserved, nil
	}
	return reapGroup(ctx, path, rec, snapshot, fields)
}

func readRecord(ctx context.Context, path string) (record, recordSnapshot, error) {
	var lastErr error
	var lastSnapshot recordSnapshot
	for attempt := range recordReadAttempts {
		rec, snapshot, err := readRecordOnce(path)
		if err == nil {
			return rec, snapshot, nil
		}
		lastErr = err
		lastSnapshot = snapshot
		if attempt == recordReadAttempts-1 {
			break
		}
		timer := time.NewTimer(recordReadRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return record{}, lastSnapshot, errors.Join(ctx.Err(), lastErr)
		case <-timer.C:
		}
	}
	return record{}, lastSnapshot, lastErr
}

func readRecordOnce(path string) (record, recordSnapshot, error) {
	file, err := os.Open(path)
	if err != nil {
		return record{}, recordSnapshot{}, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return record{}, recordSnapshot{}, err
	}
	if !before.Mode().IsRegular() {
		return record{}, recordSnapshot{info: before}, errors.New("process-group record is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxRecordSize+1))
	if err != nil {
		return record{}, recordSnapshot{info: before}, err
	}
	after, err := file.Stat()
	if err != nil {
		return record{}, recordSnapshot{info: before}, err
	}
	current, err := os.Stat(path)
	if err != nil {
		return record{}, recordSnapshot{info: after}, err
	}
	if !sameRecordFile(before, after) || !sameRecordFile(after, current) {
		return record{}, recordSnapshot{info: current}, errors.New("process-group record changed while being read")
	}
	snapshot := recordSnapshot{info: current}
	if len(data) > maxRecordSize {
		return record{}, snapshot, errors.New("process-group record is too large")
	}
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte{'{'}) {
		rec, parseErr := parseAuthenticatedRecord(data, current.ModTime())
		return rec, snapshot, parseErr
	}
	rec, err := parseLegacyRecord(data, current.ModTime())
	return rec, snapshot, err
}

func parseLegacyRecord(data []byte, writtenAt time.Time) (record, error) {
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
		contract:  legacyLineRecord,
		pgid:      pgid,
		parent:    parent,
		started:   time.Unix(startedUnix, 0),
		writtenAt: writtenAt,
		command:   command,
	}, nil
}

func parseAuthenticatedRecord(data []byte, writtenAt time.Time) (record, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var auth authenticatedRecord
	if err := decoder.Decode(&auth); err != nil {
		return record{}, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return record{}, err
	}
	if err := auth.validate(); err != nil {
		return record{}, err
	}
	return record{
		contract:  authenticatedJSONRecord,
		pgid:      auth.PGID,
		parent:    auth.Owner.PID,
		writtenAt: writtenAt,
		auth:      auth,
	}, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("record contains multiple JSON values")
		}
		return err
	}
	return nil
}

func (rec *authenticatedRecord) validate() error {
	if rec.PGID <= 1 || rec.Leader.PID != rec.PGID {
		return errors.New("invalid process-group leader")
	}
	if err := rec.Leader.validate(); err != nil {
		return fmt.Errorf("invalid process-group leader identity: %w", err)
	}
	if err := rec.Owner.validate(); err != nil {
		return fmt.Errorf("invalid process-group owner identity: %w", err)
	}
	if rec.Leader.BootID != rec.Owner.BootID {
		return errors.New("process-group record crosses boot identities")
	}
	decoded, err := hex.DecodeString(rec.Authentication)
	if err != nil || len(decoded) != groupAuthBytes {
		return errors.New("invalid process-group authentication")
	}
	return nil
}

func (identity recordedProcessIdentity) validate() error {
	if identity.PID < 1 || identity.BootID == "" || identity.StartID == 0 || identity.Executable == "" {
		return errors.New("identity is incomplete")
	}
	if filepath.Base(identity.Executable) != identity.Executable {
		return errors.New("executable identity contains a path")
	}
	return nil
}

func sameRecordFile(first, second os.FileInfo) bool {
	return os.SameFile(first, second) &&
		first.Size() == second.Size() &&
		first.ModTime().Equal(second.ModTime())
}

func recordIsUnchanged(path string, snapshot recordSnapshot) error {
	if snapshot.info == nil {
		return errors.New("record has no stable file identity")
	}
	current, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !sameRecordFile(snapshot.info, current) {
		return errors.New("record changed during reconciliation")
	}
	return nil
}

func removeRecord(path string, snapshot recordSnapshot) error {
	if err := recordIsUnchanged(path, snapshot); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func recordedCommand(summary string) string {
	separator := strings.LastIndex(summary, " <")
	if separator <= 0 || !strings.HasSuffix(summary, " args>") {
		return ""
	}
	return filepath.Base(summary[:separator])
}

func inspectLeader(pgid int) (leaderIdentity, error) {
	identity, err := inspectProcessIdentity(pgid)
	if errors.Is(err, errProcessNotFound) {
		return leaderIdentity{}, errLeaderExited
	}
	if err != nil {
		return leaderIdentity{}, err
	}
	if identity.pgid != pgid {
		return leaderIdentity{}, fmt.Errorf("pid %d belongs to process group %d", pgid, identity.pgid)
	}
	return leaderIdentity{
		parent:       identity.parent,
		started:      identity.started,
		commandNames: identity.commandNames,
	}, nil
}

func inspectProcessIdentity(pid int) (processIdentity, error) {
	first, err := inspectNativeProcess(pid)
	if err != nil {
		return processIdentity{}, err
	}
	// #nosec G115 -- record validation bounds process identifiers to signed 32-bit values.
	proc, err := process.NewProcess(int32(pid))
	if err != nil {
		if errors.Is(err, process.ErrorProcessNotRunning) {
			return processIdentity{}, errProcessNotFound
		}
		return processIdentity{}, err
	}
	startedMillis, err := proc.CreateTime()
	if err != nil {
		if errors.Is(err, process.ErrorProcessNotRunning) {
			return processIdentity{}, errProcessNotFound
		}
		return processIdentity{}, fmt.Errorf("inspect process start time: %w", err)
	}
	names := map[string]struct{}{filepath.Base(first.executable): {}}
	if argv, argvErr := proc.CmdlineSlice(); argvErr == nil && len(argv) > 0 {
		names[filepath.Base(argv[0])] = struct{}{}
	}
	if executable, executableErr := proc.Exe(); executableErr == nil && executable != "" {
		names[filepath.Base(executable)] = struct{}{}
	}
	if name, nameErr := proc.Name(); nameErr == nil && name != "" {
		names[filepath.Base(name)] = struct{}{}
	}
	second, err := inspectNativeProcess(pid)
	if err != nil {
		return processIdentity{}, err
	}
	if !sameNativeProcess(first, second) {
		return processIdentity{}, errProcessGroupIdentityChanged
	}
	return processIdentity{
		pid:          first.pid,
		pgid:         first.pgid,
		parent:       first.parent,
		started:      time.UnixMilli(startedMillis),
		bootID:       first.bootID,
		startID:      first.startID,
		executable:   filepath.Base(first.executable),
		commandNames: names,
	}, nil
}

func sameNativeProcess(first, second nativeProcessIdentity) bool {
	return first.pid == second.pid && first.pgid == second.pgid &&
		first.bootID == second.bootID && first.startID == second.startID
}

func inspectProcessGroup(ctx context.Context, pgid int) ([]processIdentity, error) {
	pids, err := process.PidsWithContext(ctx)
	if err != nil {
		return nil, err
	}
	identities := make([]processIdentity, 0)
	for _, rawPID := range pids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if rawPID <= 0 {
			continue
		}
		pid := int(rawPID)
		actualGroup, err := syscall.Getpgid(pid)
		if err != nil || actualGroup != pgid {
			continue
		}
		identity, err := inspectProcessIdentity(pid)
		if errors.Is(err, errProcessNotFound) || errors.Is(err, errProcessGroupIdentityChanged) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect process-group member %d: %w", pid, err)
		}
		if identity.pgid == pgid {
			identities = append(identities, identity)
		}
	}
	return identities, nil
}

func authenticateAuthenticatedGroup(ctx context.Context, rec *authenticatedRecord) ([]processIdentity, bool, error) {
	members, err := inspectProcessGroup(ctx, rec.PGID)
	if err != nil {
		return nil, false, err
	}
	if len(members) == 0 {
		if groupAlive(rec.PGID) {
			return nil, false, errors.New("live process group had no inspectable members")
		}
		return nil, false, nil
	}
	for _, member := range members {
		if member.pid == rec.PGID {
			return members, member.matches(&rec.Leader), nil
		}
	}
	var failures []error
	for _, member := range members {
		authenticated, err := processHasAuthentication(&member, rec.Authentication)
		if errors.Is(err, errProcessNotFound) || errors.Is(err, errProcessGroupIdentityChanged) {
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("authenticate process-group member %d: %w", member.pid, err))
			continue
		}
		if authenticated {
			return members, true, nil
		}
	}
	if len(failures) > 0 {
		return nil, false, errors.Join(failures...)
	}
	return members, false, nil
}

func processHasAuthentication(expected *processIdentity, authentication string) (bool, error) {
	value, err := readProcessGroupAuthentication(expected.pid)
	if err != nil {
		return false, err
	}
	current, err := inspectProcessIdentity(expected.pid)
	if err != nil {
		return false, err
	}
	if !current.same(expected) {
		return false, errProcessGroupIdentityChanged
	}
	return value == authentication, nil
}

func recordedOwnerAlive(owner recordedProcessIdentity) (bool, error) {
	identity, err := inspectProcessIdentity(owner.PID)
	if errors.Is(err, errProcessNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return identity.matches(&owner), nil
}

func (identity *processIdentity) matches(recorded *recordedProcessIdentity) bool {
	return identity.pid == recorded.PID && identity.bootID == recorded.BootID && identity.startID == recorded.StartID
}

func (identity *processIdentity) same(other *processIdentity) bool {
	return identity.pid == other.pid && identity.pgid == other.pgid &&
		identity.bootID == other.bootID && identity.startID == other.startID
}

func reconcileLeaderless(ctx context.Context, path string, rec *record, snapshot recordSnapshot, fields []*wool.LogField) (string, error) {
	w := wool.Get(ctx).In("processgroup.reconcile")
	authenticated, err := groupPredatesRecord(ctx, rec)
	if err != nil {
		w.Warn("could not authenticate leaderless process group",
			append(fields,
				wool.Field("disposition", "retained-leaderless-inspection-failure"),
				wool.ErrField(err))...)
		return dispositionFailed, fmt.Errorf("authenticate leaderless process group %d from record %s: %w", rec.pgid, path, err)
	}
	if !authenticated {
		if removeErr := removeRecord(path, snapshot); removeErr != nil {
			return "", fmt.Errorf("remove rejected process-group record %s: %w", path, removeErr)
		}
		w.Warn("rejected process-group record for reused leaderless group without signaling group",
			append(fields, wool.Field("disposition", "rejected-reused-leaderless-group"))...)
		return dispositionRejected, nil
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
		return dispositionPreserved, nil
	}
	return reapGroup(ctx, path, rec, snapshot, fields)
}

func groupPredatesRecord(ctx context.Context, rec *record) (bool, error) {
	members, err := inspectProcessGroup(ctx, rec.pgid)
	if err != nil {
		return false, err
	}
	for _, member := range members {
		if !member.started.After(rec.writtenAt.Add(createTimePrecision)) {
			return true, nil
		}
	}
	if len(members) == 0 && groupAlive(rec.pgid) {
		return false, errors.New("live process group had no inspectable members")
	}
	return false, nil
}

func ownerPredatesRecord(rec *record) (bool, error) {
	owner, err := inspectProcessIdentity(rec.parent)
	if errors.Is(err, errProcessNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !owner.started.After(rec.writtenAt.Add(createTimePrecision)), nil
}

func reapGroup(ctx context.Context, path string, rec *record, snapshot recordSnapshot, fields []*wool.LogField) (string, error) {
	w := wool.Get(ctx).In("processgroup.reconcile")
	if err := recordIsUnchanged(path, snapshot); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return dispositionRemoved, nil
		}
		return dispositionFailed, fmt.Errorf("verify process-group record %s before signaling: %w", path, err)
	}
	w.Warn("reaping stale managed process group",
		append(fields, wool.Field("disposition", "terminating-stale-owner"))...)
	if err := terminateGroup(ctx, rec); err != nil {
		w.Warn("failed to reap stale managed process group",
			append(fields,
				wool.Field("disposition", "reap-failed"),
				wool.ErrField(err))...)
		return dispositionFailed, fmt.Errorf("reap process group %d from record %s: %w", rec.pgid, path, err)
	}
	if err := removeRecord(path, snapshot); err != nil {
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

func terminateGroup(ctx context.Context, rec *record) error {
	if err := signalRecordGroup(ctx, rec, syscall.SIGTERM); err != nil {
		if errors.Is(err, errProcessGroupIdentityChanged) && !groupAlive(rec.pgid) {
			return nil
		}
		return err
	}
	if waitForGroupDeath(ctx, rec.pgid, sigtermGrace) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := signalRecordGroup(ctx, rec, syscall.SIGKILL); err != nil {
		if errors.Is(err, errProcessGroupIdentityChanged) && !groupAlive(rec.pgid) {
			return nil
		}
		return err
	}
	if waitForGroupDeath(ctx, rec.pgid, sigkillGrace) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("process group remained alive after SIGKILL")
}

func signalRecordGroup(ctx context.Context, rec *record, signal syscall.Signal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var (
		members       []processIdentity
		authenticated bool
		err           error
	)
	if rec.contract == authenticatedJSONRecord {
		members, authenticated, err = authenticateAuthenticatedGroup(ctx, &rec.auth)
	} else {
		members, authenticated, err = authenticateLegacyGroup(ctx, rec)
	}
	if err != nil {
		return err
	}
	if !authenticated {
		return errProcessGroupIdentityChanged
	}
	return signalProcessIdentities(ctx, members, signal)
}

func authenticateLegacyGroup(ctx context.Context, rec *record) ([]processIdentity, bool, error) {
	members, err := inspectProcessGroup(ctx, rec.pgid)
	if err != nil {
		return nil, false, err
	}
	if len(members) == 0 {
		if groupAlive(rec.pgid) {
			return nil, false, errors.New("live process group had no inspectable members")
		}
		return nil, false, nil
	}
	for _, member := range members {
		if member.pid != rec.pgid {
			continue
		}
		leader := leaderIdentity{parent: member.parent, started: member.started, commandNames: member.commandNames}
		return members, !member.started.After(rec.writtenAt.Add(createTimePrecision)) && rec.matches(leader), nil
	}
	for _, member := range members {
		if !member.started.After(rec.writtenAt.Add(createTimePrecision)) {
			return members, true, nil
		}
	}
	return members, false, nil
}

func signalProcessIdentities(ctx context.Context, identities []processIdentity, signal syscall.Signal) error {
	handles := make([]processSignalHandle, 0, len(identities))
	for _, identity := range identities {
		if err := ctx.Err(); err != nil {
			_ = closeProcessSignalHandles(handles)
			return err
		}
		handle, err := openProcessSignalHandle(&identity)
		if errors.Is(err, errProcessNotFound) || errors.Is(err, errProcessGroupIdentityChanged) {
			continue
		}
		if err != nil {
			_ = closeProcessSignalHandles(handles)
			return fmt.Errorf("open authenticated process %d: %w", identity.pid, err)
		}
		handles = append(handles, handle)
	}
	if len(handles) == 0 {
		return errProcessGroupIdentityChanged
	}
	failures := make([]error, 0, len(handles)+1)
	for _, handle := range handles {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		if err := handle.Signal(signal); err != nil && !errors.Is(err, syscall.ESRCH) {
			failures = append(failures, err)
		}
	}
	failures = append(failures, closeProcessSignalHandles(handles))
	return errors.Join(failures...)
}

func closeProcessSignalHandles(handles []processSignalHandle) error {
	failures := make([]error, 0, len(handles))
	for _, handle := range handles {
		failures = append(failures, handle.Close())
	}
	return errors.Join(failures...)
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
