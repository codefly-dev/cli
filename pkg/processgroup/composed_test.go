package processgroup

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	runnersbase "github.com/codefly-dev/core/runners/base"
)

// newDirectory is somewhere a process can run that is NOT a workspace: a
// composed module's checkout under the module cache, a store's data directory
// under ~/.codefly/data. Nothing in such a path names the workspace whose run
// put a process there, which is why attribution cannot come from it.
func newDirectory(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := enclosingWorkspace(resolved); ok {
		t.Fatalf("%s resolves to a workspace, so it cannot stand in for a checkout outside one", resolved)
	}
	return resolved
}

// startRunFrom is one process of a run: a tracked process group registered
// through the same API a runner uses, running in dir, and carrying the
// workspace whose run launched it exactly as a real one does — inherited from
// the codefly process that started the chain. Passing an empty workspace starts
// a process that records nothing, which is what a run launched by an older CLI
// looks like and must still be attributable from its working directory.
func startRunFrom(t *testing.T, workspace, dir string) int {
	t.Helper()
	port := availablePort(t)
	readyPath := filepath.Join(dir, "ready-"+strconv.Itoa(port))
	command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
	command.Env = append(withoutLaunchWorkspace(os.Environ()),
		helperRoleEnv+"=listener",
		helperPortEnv+"="+strconv.Itoa(port),
		helperReadyEnv+"="+readyPath)
	if workspace != "" {
		command.Env = append(command.Env, LaunchWorkspaceEnv+"="+workspace)
	}
	command.Dir = dir
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	tracked, err := runnersbase.StartTrackedProcessGroup(command)
	if err != nil {
		t.Fatal(err)
	}
	pgid := tracked.PGID()
	// Reap the leader the moment it exits. This test is its parent, and an
	// unreaped zombie still holds the pgid — the group would read as alive long
	// after it honoured SIGTERM, and every stop would pay the full escalation.
	reaped := make(chan struct{})
	go func() {
		_, _ = command.Process.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-reaped
	})
	waitForFile(t, readyPath)
	return pgid
}

// withoutLaunchWorkspace drops any inherited attribution, so a helper started
// without one really carries none and the fallback path is exercised for real.
func withoutLaunchWorkspace(environ []string) []string {
	kept := make([]string, 0, len(environ))
	for _, entry := range environ {
		if strings.HasPrefix(entry, LaunchWorkspaceEnv+"=") {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

// TestStopActsOnEveryProcessOfAComposedRun is the regression this attribution
// exists for.
//
// A composed run puts processes where its own workspace does not reach: a
// composed module's service runs from that module's checkout, which sits
// outside the workspace that composed it, and the store the run started runs
// from a data directory that is in no workspace at all. Attributed by working
// directory, both read as belonging to nobody — and a scoped stop, which must
// never signal what it cannot attribute, left them running while reporting
// success. They kept holding their ports, which is what the next run collides
// with.
//
// The stop here must end all three of its own processes and still leave a
// second, independent run completely alone.
func TestStopActsOnEveryProcessOfAComposedRun(t *testing.T) {
	// The registry lives under $HOME/.codefly/runs; point HOME at a temp dir so
	// this test never reads or signals anything in the developer's own registry.
	t.Setenv("HOME", t.TempDir())

	workspace := newWorkspace(t, "composing")
	moduleCheckout := newDirectory(t, "composed-module")
	storeData := newDirectory(t, "store-data")
	other := newWorkspace(t, "independent")

	run := map[string]int{
		"top-level service":         startRunFrom(t, workspace, workspace),
		"composed module's service": startRunFrom(t, workspace, moduleCheckout),
		"store the run started":     startRunFrom(t, workspace, storeData),
	}
	independent := startRunFrom(t, other, other)

	evidence, err := StopManagedProcessGroups(context.Background(), InWorkspace(workspace))
	if err != nil {
		t.Fatalf("stop scoped to %s: %v", workspace, err)
	}

	// Acting on a group removes its record, so the record is the decision.
	// Whether the process has finished dying is the listener's business.
	for what, pgid := range run {
		if recordExists(t, pgid) {
			t.Errorf("stop left the %s (%d) running: it is part of this workspace's run", what, pgid)
		}
		if containsInt(evidence.OutOfScope, pgid) {
			t.Errorf("stop reported the %s (%d) as belonging to another workspace", what, pgid)
		}
	}

	// The other half of the bargain: a second run is untouched, and said so.
	if !groupAlive(independent) {
		t.Fatalf("stop scoped to %s killed the independent run %d", workspace, independent)
	}
	if !recordExists(t, independent) {
		t.Errorf("stop removed the independent run's registry record for %d", independent)
	}
	if !containsInt(evidence.OutOfScope, independent) {
		t.Errorf("evidence does not report leaving the independent run %d alone: OutOfScope=%v", independent, evidence.OutOfScope)
	}
}

// TestWorkspaceOfProcessPrefersTheRunOverTheWorkingDirectory pins the order the
// fix depends on, both ways round: the recorded run answers even when the
// working directory says nothing, and the working directory still answers when
// no run was recorded — which is a run an older CLI started, and must stay
// attributable rather than becoming unstoppable.
func TestWorkspaceOfProcessPrefersTheRunOverTheWorkingDirectory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workspace := newWorkspace(t, "composing")
	moduleCheckout := newDirectory(t, "composed-module")

	composed := startRunFrom(t, workspace, moduleCheckout)
	found, ok := WorkspaceOfProcess(composed)
	if !ok {
		t.Fatalf("no workspace found for a composed run's process %d", composed)
	}
	if !sameDirectory(found, workspace) {
		t.Errorf("composed process attributed to %s, want the workspace that launched it, %s", found, workspace)
	}

	unrecorded := startRunFrom(t, "", workspace)
	found, ok = WorkspaceOfProcess(unrecorded)
	if !ok {
		t.Fatalf("no workspace found for %d, which records no run and runs inside one", unrecorded)
	}
	if !sameDirectory(found, workspace) {
		t.Errorf("process with no recorded run attributed to %s, want its enclosing workspace %s", found, workspace)
	}

	outside := startRunFrom(t, "", newDirectory(t, "nowhere"))
	if found, ok = WorkspaceOfProcess(outside); ok {
		t.Errorf("process %d in no workspace and with no recorded run was attributed to %s", outside, found)
	}
}

// TestScanNativeServiceOrphansAttributesToItsRun covers the field a scoped reap
// filters on. NativeServiceOrphan.Workspace was documented and never assigned,
// so every native service — every compiled service binary and every store —
// read as unattributable, and a scoped stop skipped all of them.
func TestScanNativeServiceOrphansAttributesToItsRun(t *testing.T) {
	workspace := tempWorkspace(t)
	storeData := newDirectory(t, "store-data")

	launched := startNativeServiceIn(t, nativeCacheBinary(t), workspace, storeData)
	found := waitForScannedNativeOrphan(t, launched)
	if !sameDirectory(found.Workspace, workspace) {
		t.Errorf("native service running from %s attributed to %q, want the workspace that launched it, %s", storeData, found.Workspace, workspace)
	}

	inWorkspace := startNativeServiceIn(t, nativeCacheBinary(t), "", workspace)
	found = waitForScannedNativeOrphan(t, inWorkspace)
	if !sameDirectory(found.Workspace, workspace) {
		t.Errorf("native service with no recorded run attributed to %q, want its enclosing workspace %s", found.Workspace, workspace)
	}
}

// TestScanDevServerOrphansFindsAComposedModulesDevServer covers the other half
// of what `ps` could not see: a dev server was only ever found when its working
// directory sat inside a workspace, which a composed module's frontend never
// does.
func TestScanDevServerOrphansFindsAComposedModulesDevServer(t *testing.T) {
	workspace := tempWorkspace(t)
	moduleCheckout := newDirectory(t, "composed-module")

	devServer := startDevServerHelperFor(t, moduleCheckout, workspace)
	found := waitForScannedOrphan(t, devServer)
	if !sameDirectory(found.Workspace, workspace) {
		t.Errorf("composed module's dev server attributed to %q, want the workspace that launched it, %s", found.Workspace, workspace)
	}
	if found.Cwd != moduleCheckout {
		t.Errorf("Cwd = %s, want the module checkout %s", found.Cwd, moduleCheckout)
	}
}

// startNativeServiceIn spawns a process shaped like a compiled native-mode
// service binary, running in dir and carrying the run that launched it. It is
// startNativeServiceHelper with the two things attribution turns on — a working
// directory of its own, and a recorded run — which the original helper has no
// reason to set.
func startNativeServiceIn(t *testing.T, nativeBin, workspace, dir string) int {
	t.Helper()
	return startSignatureHelper(t, nativeBin, dir, runEnvironment(workspace))
}

// startDevServerHelperFor spawns a dev-server-shaped process running in dir and
// carrying the run that launched it, and returns its pid.
func startDevServerHelperFor(t *testing.T, dir, workspace string) int {
	t.Helper()
	return startSignatureHelper(t, "next dev", dir, runEnvironment(workspace))
}

// runEnvironment is the environment a process of workspace's run carries. An
// empty workspace records nothing, which is a run an older CLI started: the
// inherited value is stripped so the fallback is exercised for real.
func runEnvironment(workspace string) []string {
	environ := withoutLaunchWorkspace(os.Environ())
	if workspace == "" {
		return environ
	}
	return append(environ, LaunchWorkspaceEnv+"="+workspace)
}

// startSignatureHelper is the shared body: a sleeping process whose argv[0] is
// the signature under test, started in dir with environ. Passing os.Environ()
// unaltered is how a test spawns the way production does — inheriting whatever
// this process recorded, with nothing set by the caller.
func startSignatureHelper(t *testing.T, argv0, dir string, environ []string) int {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestDevServerOrphanHelper$")
	command.Args[0] = argv0
	command.Env = append(environ, devServerHelperEnv+"=1")
	command.Dir = dir
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	reaped := make(chan struct{})
	go func() {
		_, _ = command.Process.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		<-reaped
	})
	return pid
}

// TestMarkLaunchWorkspaceIsWhatDescendantsInherit covers the mechanism the whole
// attribution rests on. The workspace is recorded in THIS process's own
// environment, which is what makes every process started from here carry it
// without any of them cooperating — the chain from the CLI runs through agents
// released independently of this repository, which cannot be asked to pass
// anything along.
// waitForAttributedWorkspace polls until pid can be attributed to a workspace.
//
// The attribution reads the child's own environment out of the kernel, and a
// process is visible to the kernel from fork — before exec has replaced its
// argument and environment block. Reading in that window yields no value and
// is indistinguishable from a child that inherited nothing, so asserting
// immediately after Start makes the test fail on scheduling rather than on
// behaviour. Polling is what the rest of this package already does for the
// same reason (waitForGroupLeader, waitForCodeflyOwned).
//
// Only the timing assumption is relaxed: a child that genuinely inherits
// nothing still fails, on the deadline.
func waitForAttributedWorkspace(t *testing.T, pid int) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if workspace, ok := WorkspaceOfProcess(pid); ok {
			return workspace
		}
		if time.Now().After(deadline) {
			t.Fatalf("a process started from a marked run (%d) was attributed to no workspace", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMarkLaunchWorkspaceIsWhatDescendantsInherit(t *testing.T) {
	// t.Setenv restores the variable afterwards, so marking here cannot leak
	// into another test's helpers.
	t.Setenv(LaunchWorkspaceEnv, "")
	workspace := newWorkspace(t, "composing")
	moduleCheckout := newDirectory(t, "composed-module")

	MarkLaunchWorkspace(workspace)
	if recorded := os.Getenv(LaunchWorkspaceEnv); recorded != workspace {
		t.Fatalf("recorded %q as the launching workspace, want %s", recorded, workspace)
	}

	// Nothing is passed to the child: it inherits, which is the point.
	inherited := startSignatureHelper(t, "next dev", moduleCheckout, os.Environ())
	found := waitForAttributedWorkspace(t, inherited)
	if !sameDirectory(found, workspace) {
		t.Errorf("inherited attribution is %s, want the marked workspace %s", found, workspace)
	}

	// An invocation with no workspace of its own keeps the run it belongs to
	// rather than erasing it: a codefly started from inside a run is part of it.
	MarkLaunchWorkspace("")
	if recorded := os.Getenv(LaunchWorkspaceEnv); recorded != workspace {
		t.Errorf("recording no workspace cleared the inherited run, leaving %q", recorded)
	}
}
