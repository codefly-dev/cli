package publish

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReleaseFromDetachedMainLeavesOtherWorktreeUntouched(t *testing.T) {
	dir, origin, manifest := releaseRepo(t, "0.1.0")
	original := gitIn(t, dir, "rev-parse", "HEAD")
	detached := filepath.Join(t.TempDir(), "release")
	gitIn(t, dir, "worktree", "add", "--detach", detached, "origin/main")
	fake := &fakeGitHubPullRequests{t: t, origin: origin, states: []string{"clean"}}
	engine := landingEngine(t, detached, fake)
	tag, err := engine.Release(t.Context())
	require.NoError(t, err)
	require.Equal(t, "v0.1.1", tag)
	merged := gitIn(t, origin, "rev-parse", "refs/heads/main")
	require.NotEqual(t, original, merged)
	require.Equal(t, merged, gitIn(t, origin, "rev-list", "-n1", tag))
	require.Equal(t, merged, gitIn(t, detached, "rev-parse", "HEAD"))
	require.Equal(t, "HEAD", gitIn(t, detached, "rev-parse", "--abbrev-ref", "HEAD"))
	require.Equal(t, original, gitIn(t, dir, "rev-parse", "HEAD"))
	require.Equal(t, "main", gitIn(t, dir, "branch", "--show-current"))
	require.Empty(t, gitIn(t, dir, "status", "--porcelain"))
	data, err := os.ReadFile(manifest)
	require.NoError(t, err)
	require.Equal(t, "version: 0.1.0\n", string(data))
}

func TestDetachedReleaseStillRequiresExactCleanMainAndPRLanding(t *testing.T) {
	for _, scenario := range []string{"ahead", "behind", "diverged", "dirty", "feature", "direct", "retag"} {
		t.Run(scenario, func(t *testing.T) {
			dir, origin, _ := releaseRepo(t, "0.1.0")
			detached := filepath.Join(t.TempDir(), "release")
			gitIn(t, dir, "worktree", "add", "--detach", detached, "origin/main")
			fake := &fakeGitHubPullRequests{t: t, origin: origin, states: []string{"clean"}}
			engine := landingEngine(t, detached, fake)
			want := "not in sync with origin/main"
			switch scenario {
			case "ahead":
				gitIn(t, detached, "commit", "--allow-empty", "-m", "unreviewed")
			case "behind":
				gitIn(t, dir, "commit", "--allow-empty", "-m", "new main")
				gitIn(t, dir, "push", "origin", "main")
			case "diverged":
				gitIn(t, detached, "commit", "--allow-empty", "-m", "unreviewed")
				gitIn(t, dir, "commit", "--allow-empty", "-m", "new main")
				gitIn(t, dir, "push", "origin", "main")
			case "retag":
				engine.Manifest.Mode = ModeCoreModule
				want = "not on main"
			case "dirty":
				require.NoError(t, os.WriteFile(filepath.Join(detached, "uncommitted"), []byte("x"), 0o600))
				want = "uncommitted changes"
			case "feature":
				gitIn(t, detached, "switch", "-c", "feature")
				want = "not on main"
			case "direct":
				engine.Landing = directPushLanding{}
				want = "not on main"
			}
			before := gitIn(t, origin, "rev-parse", "refs/heads/main")
			var err error
			if scenario == "retag" {
				_, err = engine.ReTag(t.Context())
			} else {
				_, err = engine.Release(t.Context())
			}
			require.ErrorContains(t, err, want)
			require.Equal(t, before, gitIn(t, origin, "rev-parse", "refs/heads/main"))
			require.Empty(t, gitIn(t, origin, "tag", "-l", "v0.1.1"))
			require.Empty(t, fake.branch, "preflight must refuse before creating a release PR")
		})
	}
}

func TestDetachedReleaseFailurePreservesSiblingEdits(t *testing.T) {
	dir, origin, manifest := releaseRepo(t, "0.1.0")
	initial := gitIn(t, dir, "rev-parse", "HEAD")
	detached := filepath.Join(t.TempDir(), "release")
	gitIn(t, dir, "worktree", "add", "--detach", detached, "origin/main")
	siblingEdit := "version: 0.1.0\n# another conversation's work\n"
	require.NoError(t, os.WriteFile(manifest, []byte(siblingEdit), 0o600))
	gitIn(t, dir, "add", manifest)
	index := gitIn(t, dir, "write-tree")
	fake := &fakeGitHubPullRequests{t: t, origin: origin, states: []string{"blocked"},
		checkRuns: `{"check_runs":[{"name":"security","status":"completed","conclusion":"failure"}]}`}
	_, err := landingEngine(t, detached, fake).Release(t.Context())
	require.ErrorContains(t, err, "security")
	require.Equal(t, initial, gitIn(t, detached, "rev-parse", "HEAD"))
	require.Empty(t, gitIn(t, detached, "status", "--porcelain"))
	require.Equal(t, initial, gitIn(t, origin, "rev-parse", "refs/heads/main"))
	require.Empty(t, gitIn(t, origin, "tag", "-l", "v0.1.1"))
	require.Equal(t, initial, gitIn(t, dir, "rev-parse", "HEAD"))
	require.Equal(t, index, gitIn(t, dir, "write-tree"))
	data, err := os.ReadFile(manifest)
	require.NoError(t, err)
	require.Equal(t, siblingEdit, string(data))
}

func TestDetachedReleaseResumesTheAcceptedUntaggedVersion(t *testing.T) {
	dir, origin, _ := releaseRepo(t, "0.1.0")
	original := gitIn(t, dir, "rev-parse", "HEAD")
	detached := filepath.Join(t.TempDir(), "release")
	gitIn(t, dir, "worktree", "add", "--detach", detached, "origin/main")
	hook := filepath.Join(origin, "hooks", "pre-receive")
	require.NoError(t, os.WriteFile(hook, []byte(`#!/bin/sh
while read -r _ _ ref; do
 case "$ref" in refs/tags/*) exit 1;; esac
done
`), 0o700))
	fake := &fakeGitHubPullRequests{t: t, origin: origin, states: []string{"clean"}}
	_, err := landingEngine(t, detached, fake).Release(t.Context())
	require.ErrorContains(t, err, "v0.1.1 is on main but its tag was not pushed")
	accepted := gitIn(t, origin, "rev-parse", "refs/heads/main")
	require.Equal(t, accepted, gitIn(t, detached, "rev-parse", "HEAD"))
	require.NoError(t, os.Remove(hook))
	retry := &fakeGitHubPullRequests{t: t, origin: origin}
	tag, err := landingEngine(t, detached, retry).Release(t.Context())
	require.NoError(t, err)
	require.Equal(t, "v0.1.1", tag)
	require.Equal(t, accepted, gitIn(t, origin, "rev-list", "-n1", tag))
	require.Equal(t, original, gitIn(t, dir, "rev-parse", "HEAD"))
	require.Empty(t, retry.branch, "resuming must not open another bump PR")
}
