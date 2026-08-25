package processgroup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
)

const (
	devServerHelperEnv = "CODEFLY_TEST_DEV_SERVER_HELPER"
	testGroupAuth      = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
)

// TestDevServerOrphanHelper is the child process re-exec'd by the process-based
// tests. It only sleeps when the marker env var is set, so it is a no-op in the
// normal test run.
func TestDevServerOrphanHelper(t *testing.T) {
	if os.Getenv(devServerHelperEnv) == "" {
		return
	}
	time.Sleep(60 * time.Second)
}

func TestIsDevServerCommand(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"next dev", []string{"node", "next", "dev"}, true},
		{"npm run dev", []string{"npm", "run", "dev", "-p", "64441"}, true},
		{"next-server child", []string{"/path/to/next-server", "(v14)"}, true},
		{"vite", []string{"node", "/repo/node_modules/.bin/vite"}, true},
		{"shell wrapping next dev", []string{"sh", "-c", "npm run prepare && next dev"}, true},
		{"unrelated node", []string{"node", "server.js"}, false},
		{"go build", []string{"go", "build", "./..."}, false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDevServerCommand(tc.argv); got != tc.want {
				t.Fatalf("isDevServerCommand(%q) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

func TestEnclosingWorkspace(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "ws")
	nested := filepath.Join(workspace, "modules", "web", "services", "frontend", "code")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, resources.WorkspaceConfigurationName), []byte("name: ws\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok := enclosingWorkspace(nested)
	if !ok || got != workspace {
		t.Fatalf("enclosingWorkspace(%q) = %q, %v; want %q, true", nested, got, ok, workspace)
	}

	outside := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := enclosingWorkspace(outside); ok {
		t.Fatalf("enclosingWorkspace(%q) unexpectedly reported a workspace", outside)
	}
}

// tempWorkspace creates a directory that looks like a codefly workspace and
// returns its symlink-resolved path (the form ScanDevServerOrphans reports).
func tempWorkspace(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, resources.WorkspaceConfigurationName), []byte("name: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// startDevServerHelper spawns a real sleeping process shaped like a dev server:
// argv[0] "next dev", cwd dir. With auth set it carries codefly's process-group
// authentication (marking it codefly-owned). joinGroup 0 makes it a new group
// leader; a positive value joins that existing group.
func startDevServerHelper(t *testing.T, dir, auth string, joinGroup int) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDevServerOrphanHelper$")
	cmd.Args[0] = "next dev"
	cmd.Env = append(os.Environ(), devServerHelperEnv+"=1")
	if auth != "" {
		cmd.Env = append(cmd.Env, groupAuthEnv+"="+auth)
	}
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: joinGroup}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	// Reap the child the moment it exits so a killed helper does not linger as a
	// zombie. In production the orphan's init parent reaps it; here the test is
	// the parent, and an unreaped zombie still answers kill(0), which would make
	// the group look alive well after it was signalled.
	reaped := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		<-reaped
	})
	return cmd
}

func waitForGroupLeader(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d did not become a group leader", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForScannedOrphan(t *testing.T, pid int) DevServerOrphan {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			t.Fatalf("dev server %d not discovered before timeout", pid)
		}
		orphans, err := ScanDevServerOrphans(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i := range orphans {
			if orphans[i].PID == pid {
				return orphans[i]
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func isAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func TestScanDevServerOrphansFindsOwnedWorkspaceDevServer(t *testing.T) {
	workspace := tempWorkspace(t)
	serviceDir := filepath.Join(workspace, "modules", "web", "services", "frontend", "code")
	if err := os.MkdirAll(serviceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wantCwd, err := filepath.EvalSymlinks(serviceDir)
	if err != nil {
		t.Fatal(err)
	}

	command := startDevServerHelper(t, serviceDir, testGroupAuth, 0)
	pid := command.Process.Pid
	found := waitForScannedOrphan(t, pid)

	if found.PGID != pid {
		t.Errorf("PGID = %d, want %d (process is its own group leader)", found.PGID, pid)
	}
	if found.Cwd != wantCwd {
		t.Errorf("Cwd = %q, want %q", found.Cwd, wantCwd)
	}
	if found.Workspace != workspace {
		t.Errorf("Workspace = %q, want %q", found.Workspace, workspace)
	}
	if found.Orphaned {
		t.Errorf("Orphaned = true, want false (parent is the live test process)")
	}
	if !found.Owned {
		t.Errorf("Owned = false, want true (process carries the group authentication)")
	}
}

func TestScanDevServerOrphansMarksUnauthenticatedExternal(t *testing.T) {
	workspace := tempWorkspace(t)
	command := startDevServerHelper(t, workspace, "", 0)
	found := waitForScannedOrphan(t, command.Process.Pid)
	if found.Owned {
		t.Errorf("Owned = true, want false (no group authentication => not codefly's)")
	}
}

func TestReapDevServerOrphansDryRunSkipsUnownedAndSupervised(t *testing.T) {
	workspace := tempWorkspace(t)
	external := startDevServerHelper(t, workspace, "", 0)              // no auth => not codefly's
	supervised := startDevServerHelper(t, workspace, testGroupAuth, 0) // owned, but leader has a live parent
	waitForScannedOrphan(t, external.Process.Pid)
	waitForScannedOrphan(t, supervised.Process.Pid)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reaped, err := ReapDevServerOrphans(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, orphan := range reaped {
		if orphan.PGID == external.Process.Pid {
			t.Errorf("would reap unowned dev server %d (not codefly's)", external.Process.Pid)
		}
		if orphan.PGID == supervised.Process.Pid {
			t.Errorf("would reap supervised dev server %d (leader still has a live parent)", supervised.Process.Pid)
		}
	}
}

func TestReapDevServerOrphansDryRunSelectsOwnedStaleGroup(t *testing.T) {
	workspace := tempWorkspace(t)
	leader := startDevServerHelper(t, workspace, testGroupAuth, 0)
	leaderPID := leader.Process.Pid
	waitForGroupLeader(t, leaderPID)

	// A second owned member joins the leader's group, then the leader dies —
	// leaving a live, codefly-owned group whose leader is gone (stale).
	member := startDevServerHelper(t, workspace, testGroupAuth, leaderPID)
	if pgid, err := syscall.Getpgid(member.Process.Pid); err != nil || pgid != leaderPID {
		t.Fatalf("member %d joined group %d, err %v; want group %d", member.Process.Pid, pgid, err, leaderPID)
	}
	if err := syscall.Kill(leaderPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for isAlive(leaderPID) {
		time.Sleep(10 * time.Millisecond)
	}
	waitForScannedOrphan(t, member.Process.Pid)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reaped, err := ReapDevServerOrphans(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	selected := false
	for _, orphan := range reaped {
		if orphan.PGID == leaderPID {
			selected = true
		}
	}
	if !selected {
		t.Fatalf("owned stale group %d was not selected for reaping", leaderPID)
	}
}

func TestKillAuthenticatedProcessGroupReapsOwnedGroup(t *testing.T) {
	workspace := tempWorkspace(t)
	command := startDevServerHelper(t, workspace, testGroupAuth, 0)
	pid := command.Process.Pid
	waitForGroupLeader(t, pid)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := killAuthenticatedProcessGroup(ctx, pid); err != nil {
		t.Fatalf("killAuthenticatedProcessGroup(%d) = %v", pid, err)
	}
	if isAlive(pid) {
		t.Fatalf("owned dev-server group %d survived reaping", pid)
	}
}

func TestKillAuthenticatedProcessGroupSparesUnownedGroup(t *testing.T) {
	workspace := tempWorkspace(t)
	command := startDevServerHelper(t, workspace, "", 0) // no auth => not codefly's
	pid := command.Process.Pid
	waitForGroupLeader(t, pid)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := killAuthenticatedProcessGroup(ctx, pid); err != nil {
		t.Fatalf("killAuthenticatedProcessGroup(%d) = %v, want nil (unowned group left untouched)", pid, err)
	}
	if !isAlive(pid) {
		t.Fatalf("unowned dev server %d was killed despite lacking codefly authentication", pid)
	}
}
