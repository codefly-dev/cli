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
	helperRoleEnv  = "CODEFLY_TEST_PROCESS_GROUP_ROLE"
	helperPortEnv  = "CODEFLY_TEST_PROCESS_GROUP_PORT"
	helperPIDEnv   = "CODEFLY_TEST_PROCESS_GROUP_PID_FILE"
	helperReadyEnv = "CODEFLY_TEST_PROCESS_GROUP_READY_FILE"
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
	pid, recordPath := spawnOrphanedListener(t, port)
	defer cleanupGroup(pid, recordPath)

	started, err := recordField(recordPath, "started")
	if err != nil {
		t.Fatal(err)
	}
	startedUnix, err := strconv.ParseInt(started, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	rewriteRecordField(t, recordPath, "started", strconv.FormatInt(startedUnix-3600, 10))

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

func recordField(path, key string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, key+"="); ok {
			return value, nil
		}
	}
	return "", fmt.Errorf("record %s has no %s field", path, key)
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
