package self

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPullRejectsOptionLikeRemoteAndBranch(t *testing.T) {
	for flag, value := range map[string]string{"remote": "--upload-pack=evil", "branch": "--help"} {
		t.Run(flag, func(t *testing.T) {
			previous, err := PullCmd.Flags().GetString(flag)
			if err != nil {
				t.Fatal(err)
			}
			if err := PullCmd.Flags().Set(flag, value); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = PullCmd.Flags().Set(flag, previous) })
			if err := PullCmd.RunE(PullCmd, nil); err == nil {
				t.Fatalf("self pull accepted %s=%q", flag, value)
			}
		})
	}
}

func TestPullRepoHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := pullRepo(ctx, t.TempDir(), "origin", "main")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pullRepo error = %v, want context cancellation", err)
	}
}

func TestGitWorktreeMarkerIsRecognized(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isGitRepo(repo) {
		t.Fatal("repository with a .git file was not recognized")
	}
}

func TestDiscoverAgentReposAllowsMissingCategories(t *testing.T) {
	targets, err := discoverAgentRepos(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 {
		t.Fatalf("unexpected targets: %+v", targets)
	}
}
