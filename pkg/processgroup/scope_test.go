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

	"github.com/codefly-dev/core/resources"
	runnersbase "github.com/codefly-dev/core/runners/base"
)

// newWorkspace is a directory a process can run in that resolves as its own
// workspace: the marker file is what enclosingWorkspace walks up looking for.
func newWorkspace(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, resources.WorkspaceConfigurationName)
	if err := os.WriteFile(marker, []byte("name: "+name+"\nlayout: modules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// t.TempDir is under /var on darwin, a symlink to /private/var, so a process
	// reports the resolved path as its cwd. Resolve here too or the scope compares
	// two spellings of one directory.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// startRunIn is one workspace's run: a tracked process group whose working
// directory is inside that workspace, registered through the same API a runner
// uses, with this test process as its live owner. That is the shape of a
// `codefly run` a stop is expected to signal — not a hand-written record, which
// would not authenticate against its leader.
func startRunIn(t *testing.T, workspace string) int {
	t.Helper()
	port := availablePort(t)
	readyPath := filepath.Join(workspace, "ready")
	command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
	command.Env = append(os.Environ(),
		helperRoleEnv+"=listener",
		helperPortEnv+"="+strconv.Itoa(port),
		helperReadyEnv+"="+readyPath)
	command.Dir = workspace
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	tracked, err := runnersbase.StartTrackedProcessGroup(command)
	if err != nil {
		t.Fatal(err)
	}
	pgid := tracked.PGID()
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_, _ = command.Process.Wait()
	})
	waitForFile(t, readyPath)
	return pgid
}

// recordExists reports whether a registry record for pgid is still on disk, in
// either the authenticated registry or the legacy root a released agent writes.
// A stop that acted on a group removes its record; a stop that left the group
// alone must leave the record too, or that workspace's own stop can no longer
// find its run.
func recordExists(t *testing.T, pgid int) bool {
	t.Helper()
	dir, err := stateDir()
	if err != nil {
		t.Fatal(err)
	}
	name := strconv.Itoa(pgid) + ".pgid"
	for _, candidate := range []string{filepath.Join(dir, authenticatedDirName, name), filepath.Join(dir, name)} {
		if _, err := os.Stat(candidate); err == nil {
			return true
		}
	}
	return false
}

// TestStopManagedProcessGroupsOnlyTouchesItsOwnWorkspace is the regression this
// scope exists for. Two independent runs, one per workspace: a stop issued in the
// first must end the first and leave the second running.
//
// Before the scope existed, a stop in either workspace killed both — and said
// nothing about it, so a machine running one workspace per checkout lost work
// whenever anyone stopped anything.
func TestStopManagedProcessGroupsOnlyTouchesItsOwnWorkspace(t *testing.T) {
	// The registry lives under $HOME/.codefly/runs; point HOME at a temp dir so
	// this test never reads or signals anything in the developer's own registry.
	t.Setenv("HOME", t.TempDir())
	first := newWorkspace(t, "first")
	second := newWorkspace(t, "second")
	firstPGID := startRunIn(t, first)
	secondPGID := startRunIn(t, second)

	evidence, err := StopManagedProcessGroups(context.Background(), InWorkspace(first))
	if err != nil {
		t.Fatalf("stop scoped to the first workspace: %v", err)
	}

	// What the stop did to its own run: acted on it, so its record is gone.
	// Whether the process has finished dying is the listener's business, not the
	// scope's, so the assertion is on the decision rather than on the signal.
	if recordExists(t, firstPGID) {
		t.Errorf("stop scoped to %s did not act on its own run %d", first, firstPGID)
	}

	// The regression itself: the other workspace's run is untouched.
	if !groupAlive(secondPGID) {
		t.Fatalf("stop scoped to %s killed the other workspace's run %d", first, secondPGID)
	}
	if !recordExists(t, secondPGID) {
		t.Errorf("stop removed the other workspace's registry record for %d", secondPGID)
	}
	// And it has to say so, because silence reads as "nothing else was running".
	if !containsInt(evidence.OutOfScope, secondPGID) {
		t.Errorf("evidence does not report leaving %d alone: OutOfScope=%v", secondPGID, evidence.OutOfScope)
	}
}

// TestStopManagedProcessGroupsAllWorkspacesStillStopsEverything keeps the escape
// hatch honest: --all is what the old behaviour is called now, not something that
// quietly became impossible.
func TestStopManagedProcessGroupsAllWorkspacesStillStopsEverything(t *testing.T) {
	// The registry lives under $HOME/.codefly/runs; point HOME at a temp dir so
	// this test never reads or signals anything in the developer's own registry.
	t.Setenv("HOME", t.TempDir())
	first := newWorkspace(t, "first")
	second := newWorkspace(t, "second")
	firstPGID := startRunIn(t, first)
	secondPGID := startRunIn(t, second)

	evidence, err := StopManagedProcessGroups(context.Background(), AllWorkspaces())
	if err != nil {
		t.Fatalf("machine-wide stop: %v", err)
	}
	for name, pgid := range map[string]int{first: firstPGID, second: secondPGID} {
		if recordExists(t, pgid) {
			t.Errorf("machine-wide stop did not act on %s run %d", name, pgid)
		}
	}
	if len(evidence.OutOfScope) != 0 {
		t.Errorf("the machine-wide scope reported groups as out of scope: %v", evidence.OutOfScope)
	}
}

// TestScopeExcludesWhatItCannotAttribute: a group whose workspace cannot be
// resolved is not "in every workspace", it is in none. A scoped stop must leave
// it alone rather than guess, which is what makes the scope safe for a registry
// whose records carry no workspace of their own.
func TestScopeExcludesWhatItCannotAttribute(t *testing.T) {
	workspace := newWorkspace(t, "only")
	scope := InWorkspace(workspace)
	if scope.Includes("") {
		t.Error("a scoped reap admitted a group it could not attribute to any workspace")
	}
	if !scope.Includes(workspace) {
		t.Error("a scoped reap excluded its own workspace")
	}
	if scope.Includes(filepath.Join(workspace, "..", "elsewhere")) {
		t.Error("a scoped reap admitted a sibling directory")
	}
	if !AllWorkspaces().Includes("") || !AllWorkspaces().Includes(workspace) {
		t.Error("the machine-wide scope must admit everything, including the unattributable")
	}
	if !AllWorkspaces().All() || scope.All() {
		t.Error("All() must distinguish the machine-wide scope from a workspace scope")
	}
}

// TestCurrentWorkspaceFindsTheEnclosingRoot covers how a command turns "where am
// I" into a scope, including from a subdirectory, which is where anyone actually
// runs stop from.
func TestCurrentWorkspaceFindsTheEnclosingRoot(t *testing.T) {
	workspace := newWorkspace(t, "root")
	nested := filepath.Join(workspace, "modules", "a", "services", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	found, ok := CurrentWorkspace(nested)
	if !ok {
		t.Fatalf("no workspace found from %s", nested)
	}
	if !sameDirectory(found, workspace) {
		t.Errorf("found workspace %s, want %s", found, workspace)
	}
	outside := t.TempDir()
	if _, ok := CurrentWorkspace(outside); ok {
		t.Errorf("found a workspace enclosing %s, which has no marker", outside)
	}
}

func containsInt(values []int, want int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
