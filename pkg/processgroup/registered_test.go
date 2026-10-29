package processgroup

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

// legacyBootID rewrites a group's registry record as an older Darwin writer
// left it: the boot id differs while the leader's pid and start are unchanged,
// which Core refuses to decide (checkBootIdentity) and keeps the record for.
func legacyBootID(t *testing.T, pgid int) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(home, ".codefly", "*", "authenticated-v2", strconv.Itoa(pgid)+".pgid"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("record for %d: %v %v", pgid, paths, err)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	// An older writer recorded one boot id for the leader and the owner alike.
	for _, field := range []string{"leader", "owner"} {
		identity, ok := record[field].(map[string]any)
		if !ok {
			t.Fatalf("record has no %s: %s", field, raw)
		}
		identity["boot_id"] = "1793000000:123456"
	}
	out, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths[0], out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// One group whose boot identity Core cannot decide is kept and named, with
// the command that recovers it, but it no longer stops the preview or the
// stop from handling every other group in the workspace (cli#1233).
func TestAnUndecidableGroupIsKeptWithoutStoppingTheRest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workspace := newWorkspace(t, "first")
	undecidable := startUnobservableRun(t, workspace, workspace)
	healthy := startUnobservableRun(t, workspace, workspace)
	legacyBootID(t, undecidable)

	preview, err := PreviewManagedProcessGroups(context.Background(), InWorkspace(workspace))
	if len(preview) != 1 || preview[0] != healthy {
		t.Fatalf("preview=%v err=%v, want only the healthy group %d", preview, err, healthy)
	}
	if err == nil || !strings.Contains(err.Error(), strconv.Itoa(undecidable)) || !strings.Contains(err.Error(), "codefly recover process-group "+strconv.Itoa(undecidable)) {
		t.Fatalf("preview error %v does not name the undecidable group and its recovery", err)
	}

	_, err = StopManagedProcessGroups(context.Background(), InWorkspace(workspace))
	if groupAlive(healthy) {
		t.Fatal("the healthy group survived the stop")
	}
	if !groupAlive(undecidable) {
		t.Fatal("the undecidable group was signaled")
	}
	if err == nil || !strings.Contains(err.Error(), "codefly recover process-group "+strconv.Itoa(undecidable)) {
		t.Fatalf("stop error %v does not name the undecidable group's recovery", err)
	}
}
