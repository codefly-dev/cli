//go:build darwin

package processgroup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Isolate the writer's pre-v0.19.7 BootID convention; leave the PID, start
// identity, executable, owner and authentication intact.
func TestOldDarwinBootIdentityRetainsLiveRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	port := availablePort(t)
	pidPath, readyPath := filepath.Join(dir, "pid"), filepath.Join(dir, "ready")
	owner := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
	owner.Env = append(os.Environ(), helperRoleEnv+"=authenticated-owner", helperPortEnv+"="+strconv.Itoa(port), helperPIDEnv+"="+pidPath, helperReadyEnv+"="+readyPath)
	owner.Stdout, owner.Stderr = io.Discard, io.Discard
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Process.Kill(); _ = owner.Wait() })
	waitForFile(t, pidPath)
	waitForFile(t, readyPath)
	pid := readPID(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	path := filepath.Join(home, ".codefly", stateDirName, "authenticated-v2", fmt.Sprintf("%d.pgid", pid))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ReapStaleProcessGroups(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("control removed current-format live record: %v", err)
	}
	assertPortHeld(t, port)
	var record authenticatedRecord
	if err := json.Unmarshal(original, &record); err != nil {
		t.Fatal(err)
	}
	boot, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		t.Fatal(err)
	}
	old := fmt.Sprintf("%d:%d", boot.Sec, boot.Usec)
	record.Leader.BootID, record.Owner.BootID = old, old
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, stop := range []bool{false, true} {
		var evidence CleanupEvidence
		var cleanupErr error
		if stop {
			evidence, cleanupErr = StopManagedProcessGroups(context.Background(), AllWorkspaces())
		} else {
			evidence, cleanupErr = ReapStaleProcessGroupsWithEvidence(context.Background(), AllWorkspaces())
		}
		if cleanupErr == nil || !strings.Contains(cleanupErr.Error(), "incompatible boot identity") {
			t.Fatalf("stop=%v error=%v", stop, cleanupErr)
		}
		if evidence.Outcome != CleanupFailed {
			t.Fatalf("stop=%v outcome=%v", stop, evidence.Outcome)
		}
		retained, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(retained, data) {
			t.Fatalf("stop=%v changed incompatible record: %v", stop, err)
		}
		assertPortHeld(t, port)
	}
}

// A run that exits sweeps the whole machine. A group it cannot decide about is
// its failure only when the group is its own.
func TestARunIsNotFailedByAnotherRunsUndecidableGroup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	port := availablePort(t)
	pidPath, readyPath := filepath.Join(dir, "pid"), filepath.Join(dir, "ready")
	owner := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
	owner.Env = append(os.Environ(), helperRoleEnv+"=authenticated-owner", helperPortEnv+"="+strconv.Itoa(port), helperPIDEnv+"="+pidPath, helperReadyEnv+"="+readyPath)
	owner.Stdout, owner.Stderr = io.Discard, io.Discard
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Process.Kill(); _ = owner.Wait() })
	waitForFile(t, pidPath)
	waitForFile(t, readyPath)
	pid := readPID(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	path := filepath.Join(home, ".codefly", stateDirName, "authenticated-v2", fmt.Sprintf("%d.pgid", pid))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record authenticatedRecord
	if err := json.Unmarshal(original, &record); err != nil {
		t.Fatal(err)
	}
	boot, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		t.Fatal(err)
	}
	old := fmt.Sprintf("%d:%d", boot.Sec, boot.Usec)
	record.Leader.BootID, record.Owner.BootID = old, old
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	_, kept, err := ReapStaleProcessGroupsAfterRun(context.Background(), func(int) bool { return false })
	if err != nil {
		t.Fatalf("another run's undecidable group failed this run: %v", err)
	}
	if len(kept) != 1 || kept[0].PGID != pid || !strings.Contains(kept[0].Error(), "incompatible boot identity") || !strings.Contains(kept[0].Error(), fmt.Sprintf("codefly recover process-group %d --preview", pid)) {
		t.Fatalf("kept = %v, want group %d named with how to recover it", kept, pid)
	}

	_, kept, err = ReapStaleProcessGroupsAfterRun(context.Background(), func(pgid int) bool { return pgid == pid })
	if err == nil || !strings.Contains(err.Error(), "incompatible boot identity") || !strings.Contains(err.Error(), fmt.Sprintf("group %d", pid)) {
		t.Fatalf("the run's own undecidable group did not fail it: %v", err)
	}
	if len(kept) != 0 {
		t.Fatalf("the run's own group was answered as somebody else's: %v", kept)
	}

	retained, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(retained, data) {
		t.Fatalf("the sweep changed the undecidable record: %v", err)
	}
	assertPortHeld(t, port)
}
