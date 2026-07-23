package self

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initRepoWithRemote creates a git repo at root/name with the given origin URL.
func initRepoWithRemote(t *testing.T, root, name, originURL string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if originURL != "" {
		run("remote", "add", "origin", originURL)
	}
}

func TestPullTargetsDiscoversCodeflyReposDynamically(t *testing.T) {
	root := t.TempDir()

	// codefly repos — discovered by origin remote, SSH and HTTPS forms.
	initRepoWithRemote(t, root, "core", "git@github.com:codefly-dev/core.git")
	initRepoWithRemote(t, root, "cli", "https://github.com/codefly-dev/cli.git")
	initRepoWithRemote(t, root, "sdk-go", "git@github.com:codefly-dev/sdk-go.git")
	// a foreign repo living alongside — must be excluded from the default set.
	initRepoWithRemote(t, root, "unrelated", "git@github.com:someone/other.git")
	// a plain directory (not a git repo) — ignored.
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	// agents/ is always considered even though it has no codefly remote.
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}

	names, err := pullTargets(context.Background(), root, false)
	if err != nil {
		t.Fatalf("pullTargets: %v", err)
	}

	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	for _, want := range []string{"core", "cli", "sdk-go", "agents"} {
		if !got[want] {
			t.Errorf("expected %q in default targets, got %v", want, names)
		}
	}
	if got["unrelated"] {
		t.Errorf("foreign repo should not be in default targets: %v", names)
	}
	if got["notes"] {
		t.Errorf("non-git dir should not be in default targets: %v", names)
	}
	// A removed repo (e.g. proto) simply never appears — nothing hardcoded.
	if got["proto"] {
		t.Errorf("nonexistent proto should not appear: %v", names)
	}
}

func TestPullTargetsAllReturnsEveryDir(t *testing.T) {
	root := t.TempDir()
	initRepoWithRemote(t, root, "core", "git@github.com:codefly-dev/core.git")
	initRepoWithRemote(t, root, "unrelated", "git@github.com:someone/other.git")
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}

	names, err := pullTargets(context.Background(), root, true)
	if err != nil {
		t.Fatalf("pullTargets --all: %v", err)
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	for _, want := range []string{"core", "unrelated", "notes"} {
		if !got[want] {
			t.Errorf("--all should include %q, got %v", want, names)
		}
	}
}
