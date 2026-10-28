package processgroup

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	runners "github.com/codefly-dev/core/runners/base"
)

func TestRegisteredWorkspaceOwner(t *testing.T) {
	pidFile := os.Getenv("CODEFLY_TEST_REGISTERED_PID_FILE")
	if pidFile == "" {
		return
	}
	child := exec.Command("env", "-i", "sleep", "60")
	child.Stdout, child.Stderr = io.Discard, io.Discard
	group, err := runners.StartTrackedProcessGroup(child)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(group.PGID())), 0600); err != nil {
		t.Fatal(err)
	}
}

func startUnobservableRun(t *testing.T, workspace, cwd string) int {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "pid")
	owner := exec.Command(os.Args[0], "-test.run=^TestRegisteredWorkspaceOwner$")
	owner.Dir = cwd
	owner.Env = append(withoutLaunchWorkspace(os.Environ()), LaunchWorkspaceEnv+"="+workspace, "CODEFLY_TEST_REGISTERED_PID_FILE="+pidFile)
	if out, err := owner.CombinedOutput(); err != nil {
		t.Fatalf("owner: %v: %s", err, out)
	}
	pid := readPID(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	deadline := time.Now().Add(5 * time.Second)
	for {
		identity, err := runners.InspectProcess(pid)
		if err == nil && identity.Executable == "sleep" {
			if _, visible := LaunchWorkspaceOfProcess(pid); !visible {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not erase its environment")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return pid
}

func TestRegisteredStopUsesPersistedWorkspaceAfterOwnerAndEnvironmentDisappear(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	first, second := newWorkspace(t, "first"), newWorkspace(t, "second")
	// Both children run from the sibling checkout. Its cwd is deliberately
	// wrong evidence for the first child's launch ownership.
	firstPID := startUnobservableRun(t, first, second)
	secondPID := startUnobservableRun(t, second, second)
	preview, err := PreviewManagedProcessGroups(context.Background(), InWorkspace(first))
	if err != nil || len(preview) != 1 || preview[0] != firstPID {
		t.Fatalf("preview=%v err=%v", preview, err)
	}
	if !groupAlive(firstPID) || !groupAlive(secondPID) {
		t.Fatal("preview signaled a group")
	}
	evidence, err := StopManagedProcessGroups(context.Background(), InWorkspace(first))
	if err != nil {
		t.Fatal(err)
	}
	if groupAlive(firstPID) {
		t.Fatal("target survived scoped stop")
	}
	if !groupAlive(secondPID) {
		t.Fatal("scoped stop killed sibling")
	}
	if _, err := runners.LookupProcessGroup(secondPID); err != nil {
		t.Fatalf("sibling registration lost: %v", err)
	}
	found := false
	for _, pgid := range evidence.RecoveredPGIDs() {
		found = found || pgid == firstPID
	}
	if !found {
		t.Fatalf("target missing from recovery evidence: %+v", evidence)
	}
}
