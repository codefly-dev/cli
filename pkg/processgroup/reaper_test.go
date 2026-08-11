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
)

const (
	helperRoleEnv    = "CODEFLY_TEST_PROCESS_GROUP_ROLE"
	helperPortEnv    = "CODEFLY_TEST_PROCESS_GROUP_PORT"
	helperPIDEnv     = "CODEFLY_TEST_PROCESS_GROUP_PID_FILE"
	helperReadyEnv   = "CODEFLY_TEST_PROCESS_GROUP_READY_FILE"
	helperReleaseEnv = "CODEFLY_TEST_PROCESS_GROUP_RELEASE_FILE"
)

func TestProcessGroupHelper(t *testing.T) {
	switch os.Getenv(helperRoleEnv) {
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
		if err := runnersbase.WritePgidFile(pid, os.TempDir(), []string{os.Args[0]}); err != nil {
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
		if err := runnersbase.WritePgidFile(pid, os.TempDir(), []string{os.Args[0]}); err != nil {
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
		if err := runnersbase.WritePgidFile(pid, os.TempDir(), []string{os.Args[0]}); err != nil {
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
		signal.Ignore(syscall.SIGTERM)
		defer signal.Reset(syscall.SIGTERM)
		if err := os.WriteFile(os.Getenv(helperReadyEnv), []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		select {}
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
	recordedAt := time.Now().Add(-time.Second)
	pid, recordPath := spawnOrphanedListener(t, port)
	defer cleanupGroup(pid, recordPath)

	rewriteRecordField(t, recordPath, "started", strconv.FormatInt(recordedAt.Unix(), 10))
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
	recordedAt := time.Now().Add(-time.Second)
	rewriteRecordField(t, recordPath, "started", strconv.FormatInt(recordedAt.Unix(), 10))
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
	if err := runnersbase.WritePgidFile(pid, os.TempDir(), []string{os.Args[0]}); err != nil {
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
	if err := runnersbase.WritePgidFile(pid, os.TempDir(), []string{os.Args[0]}); err != nil {
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
	if err := runnersbase.WritePgidFile(parentPID, os.TempDir(), []string{os.Args[0]}); err != nil {
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
		_ = runnersbase.RemovePgidFile(pid)
	}()
	if err := runnersbase.WritePgidFile(pid, os.TempDir(), []string{os.Args[0]}); err != nil {
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
	if err := terminateGroup(ctx, pid); !errors.Is(err, context.Canceled) {
		t.Fatalf("terminate with canceled context returned %v", err)
	}
	if err := syscall.Kill(-pid, 0); err != nil {
		t.Fatalf("canceled graceful shutdown force-killed the process group: %v", err)
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
