package processgroup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	runnersbase "github.com/codefly-dev/core/runners/base"
	"github.com/shirou/gopsutil/v3/process"
)

const (
	helperRoleEnv     = "CODEFLY_TEST_PROCESS_GROUP_ROLE"
	helperPortEnv     = "CODEFLY_TEST_PROCESS_GROUP_PORT"
	helperPIDEnv      = "CODEFLY_TEST_PROCESS_GROUP_PID_FILE"
	helperChildPIDEnv = "CODEFLY_TEST_PROCESS_GROUP_CHILD_PID_FILE"
	helperReadyEnv    = "CODEFLY_TEST_PROCESS_GROUP_READY_FILE"
	helperReleaseEnv  = "CODEFLY_TEST_PROCESS_GROUP_RELEASE_FILE"
	helperTermEnv     = "CODEFLY_TEST_PROCESS_GROUP_TERM_FILE"
)

func TestProcessGroupHelper(t *testing.T) {
	switch os.Getenv(helperRoleEnv) {
	case "current-owner":
		command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
		command.Env = append(os.Environ(),
			helperRoleEnv+"=legacy-agent",
			helperPortEnv+"="+os.Getenv(helperPortEnv),
			helperChildPIDEnv+"="+os.Getenv(helperChildPIDEnv),
			helperReadyEnv+"="+os.Getenv(helperReadyEnv))
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if _, err := runnersbase.StartTrackedProcessGroup(command); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv(helperPIDEnv), []byte(strconv.Itoa(command.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
		waitForFile(t, os.Getenv(helperReadyEnv))
		select {}
	case "authenticated-owner":
		command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
		command.Env = append(os.Environ(),
			helperRoleEnv+"=listener",
			helperPortEnv+"="+os.Getenv(helperPortEnv),
			helperReadyEnv+"="+os.Getenv(helperReadyEnv))
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if _, err := runnersbase.StartTrackedProcessGroup(command); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv(helperPIDEnv), []byte(strconv.Itoa(command.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
		waitForFile(t, os.Getenv(helperReadyEnv))
		select {}
	case "legacy-agent":
		command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
		command.Env = append(os.Environ(),
			helperRoleEnv+"=listener",
			helperPortEnv+"="+os.Getenv(helperPortEnv),
			helperReadyEnv+"="+os.Getenv(helperReadyEnv))
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		pid := command.Process.Pid
		if err := writeLegacyPgidFile(pid, os.TempDir(), []string{os.Args[0]}); err != nil {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv(helperChildPIDEnv), []byte(strconv.Itoa(pid)), 0o600); err != nil {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			t.Fatal(err)
		}
		select {}
	case "owner":
		command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
		command.Env = append(os.Environ(), helperRoleEnv+"=listener")
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		pid := command.Process.Pid
		if err := writeLegacyPgidFile(pid, os.TempDir(), []string{os.Args[0]}); err != nil {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv(helperPIDEnv), []byte(strconv.Itoa(pid)), 0o600); err != nil {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			t.Fatal(err)
		}
	case "listener":
		listener, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv(helperPortEnv))
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		if err := os.WriteFile(os.Getenv(helperReadyEnv), []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		stopping := make(chan os.Signal, 1)
		signal.Notify(stopping, syscall.SIGTERM, syscall.SIGINT)
		defer signal.Stop(stopping)
		<-stopping
	case "leaderless-owner":
		command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
		command.Env = append(os.Environ(),
			helperRoleEnv+"=leader-with-child",
			helperPortEnv+"="+os.Getenv(helperPortEnv),
			helperReadyEnv+"="+os.Getenv(helperReadyEnv),
			helperReleaseEnv+"="+os.Getenv(helperReleaseEnv))
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		pid := command.Process.Pid
		waitForFile(t, os.Getenv(helperReadyEnv))
		if err := writeLegacyPgidFile(pid, os.TempDir(), []string{os.Args[0]}); err != nil {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv(helperReleaseEnv), []byte("release"), 0o600); err != nil {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			t.Fatal(err)
		}
		if err := command.Wait(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv(helperPIDEnv), []byte(strconv.Itoa(pid)), 0o600); err != nil {
			t.Fatal(err)
		}
	case "leader-with-child":
		command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
		command.Env = append(os.Environ(),
			helperRoleEnv+"=listener",
			helperPortEnv+"="+os.Getenv(helperPortEnv),
			helperReadyEnv+"="+os.Getenv(helperReadyEnv))
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		waitForFile(t, os.Getenv(helperReadyEnv))
		waitForFile(t, os.Getenv(helperReleaseEnv))
	case "managed-parent":
		command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
		command.Env = append(os.Environ(),
			helperRoleEnv+"=listener",
			helperPortEnv+"="+os.Getenv(helperPortEnv),
			helperReadyEnv+"="+os.Getenv(helperReadyEnv))
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		pid := command.Process.Pid
		if err := writeLegacyPgidFile(pid, os.TempDir(), []string{os.Args[0]}); err != nil {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv(helperPIDEnv), []byte(strconv.Itoa(pid)), 0o600); err != nil {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			t.Fatal(err)
		}
		waitForFile(t, os.Getenv(helperReadyEnv))
		stopping := make(chan os.Signal, 1)
		signal.Notify(stopping, syscall.SIGTERM, syscall.SIGINT)
		defer signal.Stop(stopping)
		<-stopping
	case "ignores-term":
		if err := os.WriteFile(os.Getenv(helperReadyEnv), []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		stopping := make(chan os.Signal, 1)
		signal.Notify(stopping, syscall.SIGTERM)
		defer signal.Stop(stopping)
		for range stopping {
			if path := os.Getenv(helperTermEnv); path != "" {
				if err := os.WriteFile(path, []byte("term"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
	case "delayed-term":
		if err := os.WriteFile(os.Getenv(helperReadyEnv), []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		stopping := make(chan os.Signal, 1)
		signal.Notify(stopping, syscall.SIGTERM)
		defer signal.Stop(stopping)
		<-stopping
		if err := os.WriteFile(os.Getenv(helperTermEnv), []byte("term"), 0o600); err != nil {
			t.Fatal(err)
		}
		waitForFile(t, os.Getenv(helperReleaseEnv))
	}
}

func TestReaperCleansLegacyAgentProcessesAfterCurrentOwnerIsKilled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	port := availablePort(t)
	agentPIDPath := filepath.Join(dir, "agent-pid")
	childPIDPath := filepath.Join(dir, "child-pid")
	readyPath := filepath.Join(dir, "ready")
	owner := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
	owner.Env = append(os.Environ(),
		helperRoleEnv+"=current-owner",
		helperPortEnv+"="+strconv.Itoa(port),
		helperPIDEnv+"="+agentPIDPath,
		helperChildPIDEnv+"="+childPIDPath,
		helperReadyEnv+"="+readyPath)
	owner.Stdout = io.Discard
	owner.Stderr = io.Discard
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, agentPIDPath)
	waitForFile(t, childPIDPath)
	waitForFile(t, readyPath)
	agentPID := readPID(t, agentPIDPath)
	childPID := readPID(t, childPIDPath)
	legacyRecord := registryPath(t, childPID)
	t.Cleanup(func() {
		_ = syscall.Kill(-agentPID, syscall.SIGKILL)
		_ = syscall.Kill(-childPID, syscall.SIGKILL)
		_ = os.Remove(legacyRecord)
		_ = runnersbase.ReapStaleProcessGroups(context.Background())
	})
	assertPortHeld(t, port)
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Wait(); err == nil {
		t.Fatal("killed owner exited successfully")
	}

	if err := ReapStaleProcessGroups(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertPortAvailable(t, port)
	if err := syscall.Kill(-agentPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("orphaned agent process group %d is still alive: %v", agentPID, err)
	}
	if _, err := os.Stat(legacyRecord); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy child record still exists: %v", err)
	}
}

func TestReaperCleansAuthenticatedRootRecordAfterOwnerIsKilled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	port := availablePort(t)
	pidPath := filepath.Join(dir, "pid")
	readyPath := filepath.Join(dir, "ready")
	owner := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
	owner.Env = append(os.Environ(),
		helperRoleEnv+"=authenticated-owner",
		helperPortEnv+"="+strconv.Itoa(port),
		helperPIDEnv+"="+pidPath,
		helperReadyEnv+"="+readyPath)
	owner.Stdout = io.Discard
	owner.Stderr = io.Discard
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, pidPath)
	waitForFile(t, readyPath)
	pid := readPID(t, pidPath)
	currentRecord := filepath.Join(home, ".codefly", stateDirName, "authenticated-v1", fmt.Sprintf("%d.pgid", pid))
	rootRecord := registryPath(t, pid)
	if err := os.Rename(currentRecord, rootRecord); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = os.Remove(rootRecord)
	})
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Wait(); err == nil {
		t.Fatal("killed owner exited successfully")
	}

	assertPortHeld(t, port)
	if err := ReapStaleProcessGroups(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertPortAvailable(t, port)
	if _, err := os.Stat(rootRecord); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("authenticated root record still exists: %v", err)
	}
}

func TestReaperClearsVerifiedStaleListenerWhenOwnerPIDWasReused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	port := availablePort(t)
	pid, recordPath := spawnOrphanedListener(t, port)
	defer cleanupGroup(pid, recordPath)
	rewriteRecordField(t, recordPath, "parent", strconv.Itoa(os.Getpid()))

	assertPortHeld(t, port)
	if err := runnersbase.ReapStaleProcessGroups(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertPortHeld(t, port)
	if err := ReapStaleProcessGroups(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertPortAvailable(t, port)
	if _, err := os.Stat(recordPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reaped group record still exists: %v", err)
	}
}

func TestReaperRejectsRecordForUnrelatedReusedGroup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	port := availablePort(t)
	pid, recordPath := spawnOrphanedListener(t, port)
	defer cleanupGroup(pid, recordPath)
	leader, err := inspectLeader(pid)
	if err != nil {
		t.Fatal(err)
	}
	recordedAt := leader.started.Add(-time.Second)

	rewriteRecordField(t, recordPath, "started", strconv.FormatInt(leader.started.Unix(), 10))
	if err := os.Chtimes(recordPath, recordedAt, recordedAt); err != nil {
		t.Fatal(err)
	}

	if err := ReapStaleProcessGroups(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertPortHeld(t, port)
	if err := syscall.Kill(-pid, 0); err != nil {
		t.Fatalf("rejected process group was signaled: %v", err)
	}
	if _, err := os.Stat(recordPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected record still exists: %v", err)
	}
}

func TestReaperClearsAuthenticatedLeaderlessGroup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	port := availablePort(t)
	pid, recordPath := spawnLeaderlessListener(t, port)
	defer cleanupGroup(pid, recordPath)

	assertPortHeld(t, port)
	if err := ReapStaleProcessGroups(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertPortAvailable(t, port)
	if _, err := os.Stat(recordPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reaped leaderless group record still exists: %v", err)
	}
}

func TestReaperRejectsReusedLeaderlessGroup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	port := availablePort(t)
	pid, recordPath := spawnLeaderlessListener(t, port)
	defer cleanupGroup(pid, recordPath)
	memberStarted := processGroupMemberStart(t, pid)
	recordedAt := memberStarted.Add(-time.Second)
	rewriteRecordField(t, recordPath, "started", strconv.FormatInt(memberStarted.Unix(), 10))
	if err := os.Chtimes(recordPath, recordedAt, recordedAt); err != nil {
		t.Fatal(err)
	}

	if err := ReapStaleProcessGroups(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertPortHeld(t, port)
	if err := syscall.Kill(-pid, 0); err != nil {
		t.Fatalf("rejected leaderless process group was signaled: %v", err)
	}
	if _, err := os.Stat(recordPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected leaderless record still exists: %v", err)
	}
}

func TestReaperRetainsMalformedParentWithoutSignalingGroup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	port := availablePort(t)
	readyPath := filepath.Join(t.TempDir(), "ready")
	command := startListener(t, port, readyPath)
	pid := command.Process.Pid
	recordPath := registryPath(t, pid)
	defer func() {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		_ = command.Wait()
		_ = os.Remove(recordPath)
	}()
	if err := writeLegacyPgidFile(pid, os.TempDir(), []string{os.Args[0]}); err != nil {
		t.Fatal(err)
	}
	rewriteRecordField(t, recordPath, "parent", "not-a-pid")

	if err := ReapStaleProcessGroups(context.Background()); err == nil {
		t.Fatal("reaper accepted a malformed parent")
	}
	assertPortHeld(t, port)
	if _, err := os.Stat(recordPath); err != nil {
		t.Fatalf("malformed record was not retained: %v", err)
	}
}

func TestReaperConvergesWhenScannedRecordHasDisappeared(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := stateDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "disappeared.pgid")
	if err := os.Symlink(filepath.Join(dir, "missing-record"), path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	if err := ReapStaleProcessGroups(context.Background()); err != nil {
		t.Fatalf("reconcile disappeared record: %v", err)
	}
}

func TestReaperWaitsForRecordWriteToFinish(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	port := availablePort(t)
	readyPath := filepath.Join(t.TempDir(), "ready")
	command := startListener(t, port, readyPath)
	pid := command.Process.Pid
	recordPath := registryPath(t, pid)
	defer func() {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		_ = command.Wait()
		_ = os.Remove(recordPath)
	}()
	if err := writeLegacyPgidFile(pid, os.TempDir(), []string{os.Args[0]}); err != nil {
		t.Fatal(err)
	}
	recordData, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(recordPath, os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	writeResult := make(chan error, 1)
	go func() {
		time.Sleep(recordReadRetry)
		_, writeErr := file.Write(recordData)
		writeResult <- errors.Join(writeErr, file.Close())
	}()

	if err := ReapStaleProcessGroups(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-writeResult; err != nil {
		t.Fatal(err)
	}
	assertPortHeld(t, port)
	if _, err := os.Stat(recordPath); err != nil {
		t.Fatalf("record disappeared during concurrent write: %v", err)
	}
}

func TestReaperDoesNotRemoveReplacementRecord(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready")
	termPath := filepath.Join(dir, "term")
	releasePath := filepath.Join(dir, "release")
	command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
	command.Env = append(os.Environ(),
		helperRoleEnv+"=delayed-term",
		helperReadyEnv+"="+readyPath,
		helperTermEnv+"="+termPath,
		helperReleaseEnv+"="+releasePath)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	recordPath := registryPath(t, pid)
	if err := writeLegacyPgidFile(pid, os.TempDir(), []string{os.Args[0]}); err != nil {
		t.Fatal(err)
	}
	rewriteRecordField(t, recordPath, "parent", "2147483647")
	waitForFile(t, readyPath)
	waited := make(chan error, 1)
	go func() {
		waited <- command.Wait()
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = os.Remove(recordPath)
		select {
		case <-waited:
		case <-time.After(2 * time.Second):
		}
	})

	reaped := make(chan error, 1)
	go func() {
		reaped <- ReapStaleProcessGroups(context.Background())
	}()
	waitForFile(t, termPath)
	replacementReadyPath := filepath.Join(dir, "replacement-ready")
	replacement := startListener(t, availablePort(t), replacementReadyPath)
	replacementPID := replacement.Process.Pid
	replacementRecord := registryPath(t, replacementPID)
	if err := writeLegacyPgidFile(replacementPID, os.TempDir(), []string{os.Args[0]}); err != nil {
		t.Fatal(err)
	}
	replacementData, err := os.ReadFile(replacementRecord)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(replacementRecord); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath, replacementData, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-replacementPID, syscall.SIGTERM)
		_ = replacement.Wait()
	})
	if err := os.WriteFile(releasePath, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-reaped:
		if err == nil || !strings.Contains(err.Error(), "record changed") {
			t.Fatalf("reap with replacement record returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for reaper")
	}
	data, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("replacement record was removed: %v", err)
	}
	if !strings.Contains(string(data), fmt.Sprintf("pgid=%d", replacementPID)) {
		t.Fatalf("replacement record was overwritten: %s", data)
	}
}

func TestExpiredProcessSignalHandleDoesNotRetarget(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "ready")
	command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
	command.Env = append(os.Environ(),
		helperRoleEnv+"=listener",
		helperReadyEnv+"="+readyPath)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, readyPath)
	identity, err := inspectProcessIdentity(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := openProcessSignalHandle(&identity)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Signal(syscall.SIGKILL); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("expired process handle signal = %v, want ESRCH", err)
	}
}

func TestReaperContinuesConvergenceAfterRecordFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	port := availablePort(t)
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "pid")
	readyPath := filepath.Join(dir, "ready")
	parent := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
	parent.Env = append(os.Environ(),
		helperRoleEnv+"=managed-parent",
		helperPortEnv+"="+strconv.Itoa(port),
		helperPIDEnv+"="+pidPath,
		helperReadyEnv+"="+readyPath)
	parent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	parent.Stdout = io.Discard
	parent.Stderr = io.Discard
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	parentPID := parent.Process.Pid
	waitForFile(t, pidPath)
	childData, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(string(childData))
	if err != nil {
		t.Fatal(err)
	}
	waitForFile(t, readyPath)
	if err := writeLegacyPgidFile(parentPID, os.TempDir(), []string{os.Args[0]}); err != nil {
		t.Fatal(err)
	}
	parentRecord := registryPath(t, parentPID)
	childRecord := registryPath(t, childPID)
	rewriteRecordField(t, parentRecord, "parent", "2147483647")
	registryDir := filepath.Dir(parentRecord)
	orderedChildRecord := filepath.Join(registryDir, "000-child.pgid")
	orderedParentRecord := filepath.Join(registryDir, "999-parent.pgid")
	if err := os.Rename(childRecord, orderedChildRecord); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parentRecord, orderedParentRecord); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(registryDir, registryLockName)
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(registryDir, 0o555); err != nil {
		t.Fatal(err)
	}
	parentResult := make(chan error, 1)
	go func() {
		parentResult <- parent.Wait()
	}()
	defer func() {
		_ = os.Chmod(registryDir, 0o755)
		cleanupGroup(childPID, orderedChildRecord)
		cleanupGroup(parentPID, orderedParentRecord)
		select {
		case <-parentResult:
		case <-time.After(2 * time.Second):
			t.Error("managed parent did not exit")
		}
	}()

	if err := ReapStaleProcessGroups(context.Background()); err == nil {
		t.Fatal("reaper did not report record cleanup failures")
	}
	assertPortAvailable(t, port)
}

func TestReaperPreservesGroupOwnedByLiveParent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	port := availablePort(t)
	readyPath := filepath.Join(t.TempDir(), "ready")
	command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
	command.Env = append(os.Environ(),
		helperRoleEnv+"=listener",
		helperPortEnv+"="+strconv.Itoa(port),
		helperReadyEnv+"="+readyPath)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	recordPath := registryPath(t, pid)
	defer func() {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		_ = command.Wait()
		_ = removeLegacyPgidFile(pid)
	}()
	if err := writeLegacyPgidFile(pid, os.TempDir(), []string{os.Args[0]}); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, readyPath)

	if err := ReapStaleProcessGroups(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertPortHeld(t, port)
	if _, err := os.Stat(recordPath); err != nil {
		t.Fatalf("live owner's group record was removed: %v", err)
	}
}

func TestTerminateGroupDoesNotForceKillAfterContextCancellation(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "ready")
	command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
	command.Env = append(os.Environ(),
		helperRoleEnv+"=ignores-term",
		helperReadyEnv+"="+readyPath)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	defer func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = command.Wait()
	}()
	waitForFile(t, readyPath)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := terminateGroup(ctx, &record{pgid: pid}); !errors.Is(err, context.Canceled) {
		t.Fatalf("terminate with canceled context returned %v", err)
	}
	if err := syscall.Kill(-pid, 0); err != nil {
		t.Fatalf("canceled graceful shutdown force-killed the process group: %v", err)
	}
}

func TestReaperCancellationWhileAnotherSweepIsRunning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready")
	termPath := filepath.Join(dir, "term")
	command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
	command.Env = append(os.Environ(),
		helperRoleEnv+"=ignores-term",
		helperReadyEnv+"="+readyPath,
		helperTermEnv+"="+termPath)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	recordPath := registryPath(t, pid)
	if err := writeLegacyPgidFile(pid, os.TempDir(), []string{os.Args[0]}); err != nil {
		t.Fatal(err)
	}
	rewriteRecordField(t, recordPath, "parent", "2147483647")
	waitForFile(t, readyPath)
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = command.Wait()
		_ = os.Remove(recordPath)
	})

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() {
		firstResult <- ReapStaleProcessGroups(firstCtx)
	}()
	waitForFile(t, termPath)

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	cancelSecond()
	secondResult := make(chan error, 1)
	go func() {
		secondResult <- ReapStaleProcessGroups(secondCtx)
	}()
	select {
	case err := <-secondResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled concurrent sweep returned %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		cancelFirst()
		<-firstResult
		t.Fatal("canceled concurrent sweep remained blocked on the active sweep")
	}
	cancelFirst()
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("active sweep after cancellation returned %v", err)
	}
	if err := syscall.Kill(-pid, 0); err != nil {
		t.Fatalf("canceling the sweep killed the process group: %v", err)
	}
}

func spawnOrphanedListener(t *testing.T, port int) (int, string) {
	t.Helper()
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "pid")
	readyPath := filepath.Join(dir, "ready")
	owner := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
	owner.Env = append(os.Environ(),
		helperRoleEnv+"=owner",
		helperPortEnv+"="+strconv.Itoa(port),
		helperPIDEnv+"="+pidPath,
		helperReadyEnv+"="+readyPath)
	owner.Stdout = io.Discard
	owner.Stderr = io.Discard
	if err := owner.Run(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, pidPath)
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidData))
	if err != nil {
		t.Fatal(err)
	}
	waitForFile(t, readyPath)
	return pid, registryPath(t, pid)
}

func spawnLeaderlessListener(t *testing.T, port int) (int, string) {
	t.Helper()
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "pid")
	readyPath := filepath.Join(dir, "ready")
	releasePath := filepath.Join(dir, "release")
	owner := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
	owner.Env = append(os.Environ(),
		helperRoleEnv+"=leaderless-owner",
		helperPortEnv+"="+strconv.Itoa(port),
		helperPIDEnv+"="+pidPath,
		helperReadyEnv+"="+readyPath,
		helperReleaseEnv+"="+releasePath)
	owner.Stdout = io.Discard
	owner.Stderr = io.Discard
	if err := owner.Run(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, pidPath)
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidData))
	if err != nil {
		t.Fatal(err)
	}
	return pid, registryPath(t, pid)
}

func startListener(t *testing.T, port int, readyPath string) *exec.Cmd {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
	command.Env = append(os.Environ(),
		helperRoleEnv+"=listener",
		helperPortEnv+"="+strconv.Itoa(port),
		helperReadyEnv+"="+readyPath)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, readyPath)
	return command
}

func processGroupMemberStart(t *testing.T, pgid int) time.Time {
	t.Helper()
	pids, err := process.Pids()
	if err != nil {
		t.Fatal(err)
	}
	for _, pid := range pids {
		actualGroup, err := syscall.Getpgid(int(pid))
		if err != nil || actualGroup != pgid {
			continue
		}
		member, err := process.NewProcess(pid)
		if err != nil {
			continue
		}
		startedMillis, err := member.CreateTime()
		if err == nil {
			return time.UnixMilli(startedMillis)
		}
	}
	t.Fatalf("process group %d had no inspectable member", pgid)
	return time.Time{}
}

func registryPath(t *testing.T, pid int) string {
	t.Helper()
	dir, err := stateDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, fmt.Sprintf("%d.pgid", pid))
}

func availablePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func assertPortHeld(t *testing.T, port int) {
	t.Helper()
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err == nil {
		_ = listener.Close()
		t.Fatalf("port %d was not held", port)
	}
}

func assertPortAvailable(t *testing.T, port int) {
	t.Helper()
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("port %d was not released: %v", port, err)
	}
	_ = listener.Close()
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func cleanupGroup(pid int, recordPath string) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = os.Remove(recordPath)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func writeLegacyPgidFile(pgid int, cwd string, argv []string) error {
	dir, err := stateDir()
	if err != nil {
		return err
	}
	content := fmt.Sprintf("pgid=%d\nparent=%d\nstarted=%d\ncwd=%s\ncmd=%s\n",
		pgid, os.Getpid(), time.Now().Unix(), cwd, runnersbase.CommandSummary(argv))
	return os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.pgid", pgid)), []byte(content), 0o600)
}

func removeLegacyPgidFile(pgid int) error {
	dir, err := stateDir()
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, fmt.Sprintf("%d.pgid", pgid)))
}

func rewriteRecordField(t *testing.T, path, key, value string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	found := false
	for i, line := range lines {
		if strings.HasPrefix(line, key+"=") {
			lines[i] = key + "=" + value
			found = true
		}
	}
	if !found {
		t.Fatalf("record %s has no %s field", path, key)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
}
