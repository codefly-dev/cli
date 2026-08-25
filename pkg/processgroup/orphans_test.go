package processgroup

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
)

const devServerHelperEnv = "CODEFLY_TEST_DEV_SERVER_HELPER"

// TestDevServerOrphanHelper is the child process re-exec'd by the scan test. It
// only sleeps when the marker env var is set, so it is a no-op in the normal
// test run.
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

func TestScanDevServerOrphansFindsWorkspaceDevServer(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, resources.WorkspaceConfigurationName), []byte("name: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	serviceDir := filepath.Join(workspace, "modules", "web", "services", "frontend", "code")
	if err := os.MkdirAll(serviceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wantCwd, err := filepath.EvalSymlinks(serviceDir)
	if err != nil {
		t.Fatal(err)
	}

	command := exec.Command(os.Args[0], "-test.run=^TestDevServerOrphanHelper$")
	command.Args[0] = "next dev"
	command.Env = append(os.Environ(), devServerHelperEnv+"=1")
	command.Dir = serviceDir
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_, _ = command.Process.Wait()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var found *DevServerOrphan
	for found == nil {
		if err := ctx.Err(); err != nil {
			t.Fatalf("dev server %d not discovered before timeout", pid)
		}
		orphans, err := ScanDevServerOrphans(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i := range orphans {
			if orphans[i].PID == pid {
				found = &orphans[i]
				break
			}
		}
		if found == nil {
			time.Sleep(50 * time.Millisecond)
		}
	}

	if found.PGID != pid {
		t.Errorf("PGID = %d, want %d (process is its own group leader)", found.PGID, pid)
	}
	if found.Cwd != wantCwd {
		t.Errorf("Cwd = %q, want %q", found.Cwd, wantCwd)
	}
	wantWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if found.Workspace != wantWorkspace {
		t.Errorf("Workspace = %q, want %q", found.Workspace, wantWorkspace)
	}
	if found.Orphaned {
		t.Errorf("Orphaned = true, want false (parent is the live test process)")
	}
}

func TestReapDevServerOrphansSkipsTrackedServers(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, resources.WorkspaceConfigurationName), []byte("name: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(os.Args[0], "-test.run=^TestDevServerOrphanHelper$")
	command.Args[0] = "next dev"
	command.Env = append(os.Environ(), devServerHelperEnv+"=1")
	command.Dir = workspace
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_, _ = command.Process.Wait()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	reaped, err := ReapDevServerOrphans(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, orphan := range reaped {
		if orphan.PID == pid {
			t.Fatalf("reaped tracked dev server %d whose parent is still alive", pid)
		}
	}
	if err := command.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("tracked dev server %d was killed: %v", pid, err)
	}
}
