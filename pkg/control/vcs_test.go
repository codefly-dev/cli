package control

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initGitRepo creates a real git repo with one commit and returns its dir.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	run("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "initial")
	return dir
}

func TestGitStatusCleanThenDirty(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	status, err := New().GitStatus(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if status.Branch != "main" {
		t.Errorf("branch = %q, want main", status.Branch)
	}
	if status.Dirty {
		t.Errorf("repo should be clean after commit, got Changed=%v", status.Changed)
	}

	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err = New().GitStatus(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Dirty {
		t.Error("repo should be dirty after adding a file")
	}
	found := false
	for _, c := range status.Changed {
		if c == "new.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("Changed = %v, want it to include new.txt", status.Changed)
	}
	// Per-file detail: new.txt is untracked ("??"), not staged.
	var fileOK bool
	for _, f := range status.Files {
		if f.Path == "new.txt" {
			fileOK = true
			if f.Code != "??" || f.Staged {
				t.Errorf("new.txt file status = %+v, want code ?? and not staged", f)
			}
		}
	}
	if !fileOK {
		t.Errorf("Files = %+v, want an entry for new.txt", status.Files)
	}
}

func TestGitCommitAndLog(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	commit, err := New().GitCommit(ctx, GitCommitRequest{Dir: dir, Message: "add feature", Paths: []string{"feature.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if commit.SHA == "" {
		t.Error("commit SHA is empty")
	}

	log, err := New().GitLog(ctx, GitLogRequest{Dir: dir, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 2 {
		t.Fatalf("log has %d commits, want 2", len(log))
	}
	if log[0].Message != "add feature" {
		t.Errorf("newest commit message = %q, want %q", log[0].Message, "add feature")
	}
	if log[0].Author != "Test" {
		t.Errorf("author = %q, want Test", log[0].Author)
	}
	if log[0].ShortHash == "" || !strings.HasPrefix(log[0].SHA, log[0].ShortHash) {
		t.Errorf("short hash %q is not a prefix of %q", log[0].ShortHash, log[0].SHA)
	}
	if log[0].Date == "" {
		t.Error("commit date is empty")
	}
}
