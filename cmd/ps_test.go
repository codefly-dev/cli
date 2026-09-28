package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/processgroup"
	"github.com/codefly-dev/core/resources"
)

const psHelperEnv = "CODEFLY_TEST_PS_HELPER"

// TestPsHelper is the child process the listing test re-execs. It only sleeps
// when the marker is set, so it is a no-op in a normal test run.
func TestPsHelper(t *testing.T) {
	if os.Getenv(psHelperEnv) == "" {
		return
	}
	time.Sleep(60 * time.Second)
}

// psWorkspace is a directory that resolves as a codefly workspace, in its
// symlink-resolved form — the form a scanned process reports.
func psWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, resources.WorkspaceConfigurationName), []byte("name: composing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// psDirectory is somewhere outside any workspace: a composed module's checkout
// under the module cache, a store's data directory under ~/.codefly/data.
func psDirectory(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// startPsHelper spawns a sleeping process with the argv[0] a scan matches on,
// running in dir and carrying the workspace whose run launched it, exactly as a
// process started by that run inherits it.
func startPsHelper(t *testing.T, argv0, workspace, dir string) int {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestPsHelper$")
	command.Args[0] = argv0
	command.Env = append(os.Environ(), psHelperEnv+"=1", processgroup.LaunchWorkspaceEnv+"="+workspace)
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

// TestPsListsEveryProcessOfAComposedRun is what `codefly ps` owes someone about
// to run `codefly stop`: the complete set the stop will act on.
//
// A composed run's processes are the ones it used to miss. Its compiled service
// binaries and the store it started were never scanned at all — only dev
// servers were — and a composed module's frontend runs from that module's
// checkout, outside the workspace that composed it, so the one scan there was
// skipped it too. What was left was the workspace's own top-level dev servers,
// which is a fraction of the run, presented as all of it.
func TestPsListsEveryProcessOfAComposedRun(t *testing.T) {
	workspace := psWorkspace(t)
	moduleCheckout := psDirectory(t, "composed-module")
	storeData := psDirectory(t, "store-data")

	nativeBinary := filepath.Join(moduleCheckout, "code", ".cache", "native", "a1b2c3d4")
	want := map[int]string{
		startPsHelper(t, "next dev", workspace, workspace):        "the workspace's own dev server",
		startPsHelper(t, "next dev", workspace, moduleCheckout):   "the composed module's dev server",
		startPsHelper(t, nativeBinary, workspace, moduleCheckout): "the composed module's compiled service",
		startPsHelper(t, "postgres", workspace, storeData):        "the store the run started",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	processes := waitForPsProcesses(t, ctx, workspace, len(want))

	found := make(map[int]psProcess, len(processes))
	for _, process := range processes {
		found[process.PID] = process
	}
	for pid, what := range want {
		process, ok := found[pid]
		if !ok {
			t.Errorf("ps did not list %s (%d)", what, pid)
			continue
		}
		if process.Workspace != workspace {
			t.Errorf("%s (%d) listed under workspace %q, want %s", what, pid, process.Workspace, workspace)
		}
	}
	// Scoping is the other half: this workspace's listing is only this
	// workspace's run, which is what makes it the set `stop` acts on.
	for _, process := range processes {
		if _, ok := want[process.PID]; !ok {
			t.Errorf("ps listed %d (%s), which no run of %s launched", process.PID, process.Command, workspace)
		}
	}
}

// waitForPsProcesses polls the listing until every helper has been observed, so
// the test does not race a process that has started but not yet exec'd into its
// final argv and environment.
func waitForPsProcesses(t *testing.T, ctx context.Context, workspace string, count int) []psProcess {
	t.Helper()
	var processes []psProcess
	for {
		scanned, err := scanRunProcesses(ctx, processgroup.InWorkspace(workspace))
		if err == nil {
			processes = scanned
			if len(processes) >= count {
				return processes
			}
		} else if ctx.Err() == nil {
			t.Fatal(err)
		}
		if ctx.Err() != nil {
			// Report what the listing DID hold, not the deadline: the deadline
			// is how a missing process shows up, never the finding itself.
			t.Fatalf("only %d of the run's %d processes were listed: %s", len(processes), count, psSummary(processes))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func psSummary(processes []psProcess) string {
	var parts []string
	for _, process := range processes {
		parts = append(parts, process.Kind+" "+process.Command)
	}
	return strings.Join(parts, ", ")
}

// TestRootOptionsRecordTheLaunchWorkspace covers the wiring rather than the
// mechanism: nothing downstream can inherit an attribution the CLI never
// records, and every command goes through these options before it starts
// anything.
func TestRootOptionsRecordTheLaunchWorkspace(t *testing.T) {
	// t.Setenv restores the variable afterwards, so this cannot leak into
	// another test's helper processes.
	t.Setenv(processgroup.LaunchWorkspaceEnv, "")
	workspace := psWorkspace(t)
	// A subdirectory, because that is where anyone actually runs a codefly
	// command from — the recorded value has to be the workspace root.
	nested := filepath.Join(workspace, "modules", "billing", "services", "api")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)

	if err := applyRootOptions(); err != nil {
		t.Fatal(err)
	}
	if recorded := os.Getenv(processgroup.LaunchWorkspaceEnv); recorded != workspace {
		t.Fatalf("recorded %q as the launching workspace, want the root %s", recorded, workspace)
	}
}

// TestGatewayDirectoryOverridesTheLaunchWorkspace covers the one command that
// serves a workspace it was pointed at rather than the one it was started in.
// Its runs would otherwise be attributed to wherever the gateway happened to be
// launched from, which a stop in the served workspace cannot match.
func TestGatewayDirectoryOverridesTheLaunchWorkspace(t *testing.T) {
	t.Setenv(processgroup.LaunchWorkspaceEnv, "")
	served := psWorkspace(t)
	t.Chdir(psDirectory(t, "started-elsewhere"))

	markLaunchWorkspaceFrom(served)
	if recorded := os.Getenv(processgroup.LaunchWorkspaceEnv); recorded != served {
		t.Fatalf("recorded %q, want the workspace the gateway serves, %s", recorded, served)
	}
}
