package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/require"
)

// fakeGitHubPullRequests speaks enough of the pull-request API for the release
// landing, and performs the merge for real in the bare origin repo: a squash
// merge is a new commit whose tree is the branch's and whose parent is main,
// which is exactly what the landing has to tag afterwards.
type fakeGitHubPullRequests struct {
	t      *testing.T
	origin string

	// states is the mergeable_state sequence served to successive polls; the
	// last entry repeats once exhausted.
	states []string
	// checkRuns is the check-runs payload served for the head SHA.
	checkRuns    string
	commitStatus string
	// checkRunsStatus, when non-zero, is served instead of checkRuns.
	checkRunsStatus int
	// mergeFailsAfterMerging reproduces a merge that commits server-side and
	// still fails its caller.
	mergeFailsAfterMerging bool
	// afterChecks runs once the check-runs response has been served, which is
	// a point reached only from inside the wait for the pull request. Tests
	// that need the publish budget to expire *there* cancel from here rather
	// than racing a wall clock.
	afterChecks func()

	mu          sync.Mutex
	polls       int
	branch      string
	title       string
	base        string
	merged      bool
	commitTitle string
	mergeMethod string
}

func (f *fakeGitHubPullRequests) client(t *testing.T) *github.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/codefly-dev/cli/pulls", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		var body struct{ Title, Head, Base string }
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		f.mu.Lock()
		f.branch, f.title, f.base = body.Head, body.Title, body.Base
		f.mu.Unlock()
		fmt.Fprintf(w, `{"number":7,"head":{"sha":%q}}`, f.headSHA())
	})
	mux.HandleFunc("/repos/codefly-dev/cli/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		state := f.states[min(f.polls, len(f.states)-1)]
		f.polls++
		merged := f.merged
		f.mu.Unlock()
		fmt.Fprintf(w, `{"number":7,"merged":%t,"mergeable_state":%q,"head":{"sha":%q}}`,
			merged, state, f.headSHA())
	})
	mux.HandleFunc("/repos/codefly-dev/cli/pulls/7/merge", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPut, r.Method)
		var body struct {
			CommitTitle string `json:"commit_title"`
			MergeMethod string `json:"merge_method"`
			SHA         string `json:"sha"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, f.headSHA(), body.SHA, "merge must be pinned to the head it inspected")
		f.mu.Lock()
		f.commitTitle, f.mergeMethod, f.merged = body.CommitTitle, body.MergeMethod, true
		f.mu.Unlock()
		sha := f.squashOntoMain(body.CommitTitle)
		if f.mergeFailsAfterMerging {
			http.Error(w, "server error", http.StatusBadGateway)
			return
		}
		fmt.Fprintf(w, `{"merged":true,"sha":%q}`, sha)
	})
	mux.HandleFunc("/repos/codefly-dev/cli/commits/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			if f.commitStatus == "" {
				fmt.Fprint(w, `{"total_count":0,"state":"pending","statuses":[]}`)
			} else {
				fmt.Fprint(w, f.commitStatus)
			}
			return
		}
		if f.checkRunsStatus != 0 {
			http.Error(w, "server error", f.checkRunsStatus)
			if f.afterChecks != nil {
				f.afterChecks()
			}
			return
		}
		if f.checkRuns == "" {
			fmt.Fprint(w, `{"total_count":1,"check_runs":[{"name":"build","status":"completed","conclusion":"success"}]}`)
		} else {
			fmt.Fprint(w, f.checkRuns)
		}
		if f.afterChecks != nil {
			f.afterChecks()
		}
	})

	ts := httptest.NewServer(mux)
	// A request aborted by a cancelled context leaves its connection non-idle,
	// and plain Close waits on it — tens of seconds of it. Drop the
	// connections first so teardown does not dominate the test's runtime.
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	base := ts.URL + "/"
	client, err := github.NewClient(github.WithURLs(&base, &base))
	require.NoError(t, err)
	return client
}

func (f *fakeGitHubPullRequests) headSHA() string {
	f.mu.Lock()
	branch := f.branch
	f.mu.Unlock()
	if branch == "" {
		return ""
	}
	return f.gitOrigin("rev-parse", "refs/heads/"+branch)
}

// squashOntoMain replays the branch as one commit on top of main, the way a
// squash merge does, and returns the resulting SHA.
func (f *fakeGitHubPullRequests) squashOntoMain(title string) string {
	tree := f.gitOrigin("rev-parse", "refs/heads/"+f.branch+"^{tree}")
	parent := f.gitOrigin("rev-parse", "refs/heads/main")
	squashed := f.gitOrigin("commit-tree", tree, "-p", parent, "-m", title)
	f.gitOrigin("update-ref", "refs/heads/main", squashed)
	return squashed
}

func (f *fakeGitHubPullRequests) gitOrigin(args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", f.origin}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.Output()
	require.NoError(f.t, err, "git %v in origin", args)
	return strings.TrimSpace(string(out))
}

// releaseRepo stands up a working repo with a pushed manifest and returns it
// with its bare origin.
func releaseRepo(t *testing.T, version string) (dir, origin, manifest string) {
	t.Helper()
	dir = t.TempDir()
	origin = initReleaseOrigin(t, dir)
	manifest = filepath.Join(dir, "pkg", "cli", "info.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(manifest), 0o755))
	require.NoError(t, os.WriteFile(manifest, []byte("version: "+version+"\n"), 0o600))
	gitIn(t, dir, "add", manifest)
	gitIn(t, dir, "commit", "-m", "add manifest")
	gitIn(t, dir, "push", "origin", "main")
	return dir, origin, manifest
}

func initReleaseOrigin(t *testing.T, dir string) string {
	t.Helper()
	origin := t.TempDir()
	gitIn(t, origin, "init", "--bare", "-b", "main")
	for _, cfg := range [][2]string{{"receive.autogc", "false"}, {"gc.auto", "0"}, {"maintenance.auto", "false"}} {
		gitIn(t, origin, "config", cfg[0], cfg[1])
	}
	gitIn(t, dir, "init", "-b", "main")
	for _, cfg := range [][2]string{
		{"gc.auto", "0"}, {"maintenance.auto", "false"},
		{"commit.gpgsign", "false"}, {"tag.gpgsign", "false"},
		{"user.email", "test@example.com"}, {"user.name", "Test"},
	} {
		gitIn(t, dir, "config", cfg[0], cfg[1])
	}
	gitIn(t, dir, "remote", "add", "origin", origin)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("init\n"), 0o600))
	gitIn(t, dir, "add", "seed.txt")
	gitIn(t, dir, "commit", "-m", "seed")
	gitIn(t, dir, "push", "-u", "origin", "main")
	return origin
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v:\n%s", args, out)
	return strings.TrimSpace(string(out))
}

func landingEngine(t *testing.T, dir string, fake *fakeGitHubPullRequests) *Engine {
	t.Helper()
	m, err := Detect(dir)
	require.NoError(t, err)
	return &Engine{
		Manifest: m,
		BumpType: "patch",
		WorkDir:  dir,
		Landing: &pullRequestLanding{
			client: fake.client(t), owner: "codefly-dev", repo: "cli", poll: time.Millisecond,
		},
	}
}

func TestEngine_Release_LandsTheBumpThroughAPullRequest(t *testing.T) {
	dir, origin, manifest := releaseRepo(t, "0.1.0")
	fake := &fakeGitHubPullRequests{t: t, origin: origin, states: []string{"blocked", "clean"}}

	tag, err := landingEngine(t, dir, fake).Release(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v0.1.1", tag)

	require.Equal(t, "release-0.1.1", fake.branch, "the bump lands on a short-lived release branch")
	require.Equal(t, "main", fake.base)
	require.Equal(t, "release: v0.1.1", fake.title)
	require.Equal(t, "squash", fake.mergeMethod)
	require.Equal(t, "release: v0.1.1", fake.commitTitle,
		"main must carry the same release subject a direct push produced")
	require.Greater(t, fake.polls, 1, "publish must wait while the pull request is blocked")

	mergedSHA := gitIn(t, origin, "rev-parse", "refs/heads/main")
	require.Equal(t, mergedSHA, gitIn(t, origin, "rev-list", "-n1", tag),
		"the tag must name the commit main actually carries, not the local one it was squashed from")
	require.Equal(t, mergedSHA, gitIn(t, dir, "rev-parse", "HEAD"),
		"the local checkout must end on the merged commit")

	contents, err := os.ReadFile(manifest)
	require.NoError(t, err)
	require.Equal(t, "version: 0.1.1\n", string(contents))

	branches := gitIn(t, origin, "branch", "--list", "release-0.1.1")
	require.Empty(t, branches, "the release branch must be cleaned up after the merge")
}

func TestEngine_Release_RedPullRequest_PublishesNothing(t *testing.T) {
	dir, origin, manifest := releaseRepo(t, "0.1.0")
	headBefore := gitIn(t, dir, "rev-parse", "HEAD")
	fake := &fakeGitHubPullRequests{
		t: t, origin: origin, states: []string{"blocked"},
		checkRuns: `{"total_count":2,"check_runs":[
			{"name":"race","status":"completed","conclusion":"success"},
			{"name":"control-integration","status":"completed","conclusion":"failure"}]}`,
	}

	_, err := landingEngine(t, dir, fake).Release(context.Background())
	require.ErrorContains(t, err, "is red (control-integration)")

	contents, readErr := os.ReadFile(manifest)
	require.NoError(t, readErr)
	require.Equal(t, "version: 0.1.0\n", string(contents), "a refused bump must not survive locally")
	require.Equal(t, headBefore, gitIn(t, dir, "rev-parse", "HEAD"))
	require.Equal(t, headBefore, gitIn(t, origin, "rev-parse", "refs/heads/main"))
	require.Empty(t, gitIn(t, origin, "tag", "-l", "v0.1.1"))
	require.Empty(t, gitIn(t, origin, "branch", "--list", "release-0.1.1"))
}

func TestEngine_Release_ConflictingPullRequest_PublishesNothing(t *testing.T) {
	dir, origin, _ := releaseRepo(t, "0.1.0")
	fake := &fakeGitHubPullRequests{t: t, origin: origin, states: []string{"dirty"}}

	_, err := landingEngine(t, dir, fake).Release(context.Background())
	require.ErrorContains(t, err, "conflicts with main")
	require.Empty(t, gitIn(t, origin, "tag", "-l", "v0.1.1"))
	require.Empty(t, gitIn(t, origin, "branch", "--list", "release-0.1.1"))
}

// TestEngine_Release_FinishesABumpThatLandedWithoutItsTag covers the one window
// a pull-request landing is not atomic in: the merge succeeded, the tag push
// did not. Re-running publish must finish that release rather than bump past it
// and burn the version.
func TestEngine_Release_FinishesABumpThatLandedWithoutItsTag(t *testing.T) {
	dir, origin, manifest := releaseRepo(t, "0.1.0")
	hook := filepath.Join(origin, "hooks", "pre-receive")
	require.NoError(t, os.WriteFile(hook, []byte(`#!/bin/sh
while read -r _ _ ref; do
  case "$ref" in refs/tags/*) exit 1;; esac
done
exit 0
`), 0o755))

	fake := &fakeGitHubPullRequests{t: t, origin: origin, states: []string{"clean"}}
	_, err := landingEngine(t, dir, fake).Release(context.Background())
	require.ErrorContains(t, err, "v0.1.1 is on main but its tag was not pushed")

	mergedSHA := gitIn(t, origin, "rev-parse", "refs/heads/main")
	contents, readErr := os.ReadFile(manifest)
	require.NoError(t, readErr)
	require.Equal(t, "version: 0.1.1\n", string(contents), "the bump is on main; it must not be reverted")

	require.NoError(t, os.Remove(hook))
	tag, err := landingEngine(t, dir, &fakeGitHubPullRequests{t: t, origin: origin}).Release(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v0.1.1", tag, "the re-run finishes the stranded release instead of cutting v0.1.2")
	require.Equal(t, mergedSHA, gitIn(t, origin, "rev-list", "-n1", tag))
	require.Equal(t, mergedSHA, gitIn(t, origin, "rev-parse", "refs/heads/main"),
		"finishing a stranded release must not add another commit to main")
}

func TestEngine_Release_DryRunReportsAStrandedRelease(t *testing.T) {
	dir, origin, _ := releaseRepo(t, "0.1.0")
	gitIn(t, dir, "commit", "--allow-empty", "-m", "release: v0.1.0")
	gitIn(t, dir, "push", "origin", "main")

	engine := landingEngine(t, dir, &fakeGitHubPullRequests{t: t, origin: origin})
	engine.DryRun = true
	tag, err := engine.Release(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v0.1.0", tag, "dry-run must report the release that needs finishing, not a new bump")
}

func TestOriginRepository(t *testing.T) {
	for _, tc := range []struct {
		remote      string
		owner, repo string
		wantErr     bool
	}{
		{remote: "git@github.com:codefly-dev/cli.git", owner: "codefly-dev", repo: "cli"},
		{remote: "https://github.com/codefly-dev/cli", owner: "codefly-dev", repo: "cli"},
		{remote: "ssh://git@github.com/codefly-dev/cli.git", owner: "codefly-dev", repo: "cli"},
		{remote: "https://gitlab.com/codefly-dev/cli.git", wantErr: true},
	} {
		t.Run(tc.remote, func(t *testing.T) {
			dir := t.TempDir()
			gitIn(t, dir, "init", "-b", "main")
			gitIn(t, dir, "remote", "add", "origin", tc.remote)
			owner, repo, err := originRepository(context.Background(), dir)
			if tc.wantErr {
				require.ErrorContains(t, err, "unrecognized GitHub remote")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.owner, owner)
			require.Equal(t, tc.repo, repo)
		})
	}
}

func TestIsReleaseCommitFor(t *testing.T) {
	require.True(t, isReleaseCommitFor("release: v0.1.1", "v0.1.1"))
	require.True(t, isReleaseCommitFor("release: v0.1.1 (#742)", "v0.1.1"))
	require.False(t, isReleaseCommitFor("release: v0.1.11", "v0.1.1"),
		"a longer version must not read as the one being released")
	require.False(t, isReleaseCommitFor("fix: release: v0.1.1", "v0.1.1"))
}

// TestEngine_Release_ExhaustedBudgetStillUnwinds covers the failure the wait
// makes routine: CI outlasts the budget, so every unwind step runs after the
// context is already done. On the caller's context exec refuses to start those
// git commands at all, which left the release commit on local main, the bumped
// manifest committed, and the release branch on origin — wedging the next
// publish at its in-sync pre-flight gate.
func TestEngine_Release_ExhaustedBudgetStillUnwinds(t *testing.T) {
	dir, origin, manifest := releaseRepo(t, "0.1.0")
	headBefore := gitIn(t, dir, "rev-parse", "HEAD")
	fake := &fakeGitHubPullRequests{t: t, origin: origin, states: []string{"blocked"}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Expire the budget from inside the wait rather than after a fixed
	// duration: a wall-clock deadline races pre-flight and lands somewhere
	// else entirely when the machine is loaded.
	var once sync.Once
	fake.afterChecks = func() { once.Do(cancel) }

	_, err := landingEngine(t, dir, fake).Release(ctx)
	require.ErrorContains(t, err, "publish budget ran out")

	contents, readErr := os.ReadFile(manifest)
	require.NoError(t, readErr)
	require.Equal(t, "version: 0.1.0\n", string(contents),
		"the bump must be restored even though the budget that aborted it is gone")
	require.Equal(t, headBefore, gitIn(t, dir, "rev-parse", "HEAD"),
		"the local release commit must be removed, or the next publish fails its in-sync gate")
	require.Empty(t, gitIn(t, origin, "branch", "--list", "release-0.1.1"),
		"the release branch must be deleted, or the next attempt at this version cannot push it")
	require.Empty(t, gitIn(t, origin, "tag", "-l", "v0.1.1"))
}

// TestEngine_Release_ResumeRebuildsWhatAfterPushShips pins that finishing a
// stranded release re-runs BeforeCommit. AfterPush ships what BeforeCommit
// produces, so resuming without it published an empty release that verified
// clean because there was nothing in it to verify.
func TestEngine_Release_ResumeRebuildsWhatAfterPushShips(t *testing.T) {
	dir, origin, _ := releaseRepo(t, "0.1.0")
	fake := &fakeGitHubPullRequests{t: t, origin: origin, states: []string{"clean"}}

	var order []string
	engine := landingEngine(t, dir, fake)
	engine.BeforeCommit = func(context.Context, string) error {
		order = append(order, "before")
		return nil
	}
	engine.AfterPush = func(context.Context, string) error {
		order = append(order, "after")
		return errors.New("tag pushed, upload refused")
	}
	_, err := engine.Release(context.Background())
	require.ErrorContains(t, err, "upload refused")
	require.Equal(t, []string{"before", "after"}, order)
	// The tag landed, so the bump is no longer strandable — strip it to put the
	// repo back in the state a failed tag push leaves behind.
	gitIn(t, origin, "tag", "-d", "v0.1.1")
	gitIn(t, dir, "tag", "-d", "v0.1.1")

	order = nil
	resumed := landingEngine(t, dir, &fakeGitHubPullRequests{t: t, origin: origin})
	resumed.BeforeCommit = func(context.Context, string) error {
		order = append(order, "before")
		return nil
	}
	resumed.AfterPush = func(context.Context, string) error {
		order = append(order, "after")
		return nil
	}
	tag, err := resumed.Release(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v0.1.1", tag)
	require.Equal(t, []string{"before", "after"}, order,
		"a resume must rebuild the artifacts AfterPush uploads, not publish an empty release")
}

// TestEngine_Release_ReclaimsALeftoverReleaseBranch covers the retry a failed
// attempt used to wedge: the branch it left on origin rejected the next push as
// a non-fast-forward, with a raw git error naming no remedy.
func TestEngine_Release_ReclaimsALeftoverReleaseBranch(t *testing.T) {
	dir, origin, _ := releaseRepo(t, "0.1.0")
	gitIn(t, dir, "push", "origin", "HEAD:refs/heads/release-0.1.1")
	gitIn(t, dir, "commit", "--allow-empty", "-m", "unrelated work")
	gitIn(t, dir, "push", "origin", "main")

	fake := &fakeGitHubPullRequests{t: t, origin: origin, states: []string{"clean"}}
	tag, err := landingEngine(t, dir, fake).Release(context.Background())
	require.NoError(t, err, "a leftover release branch must not wedge the retry")
	require.Equal(t, "v0.1.1", tag)
	require.Equal(t, gitIn(t, origin, "rev-parse", "refs/heads/main"), gitIn(t, origin, "rev-list", "-n1", tag))
}

// TestEngine_Release_MergeErrorAfterServerSideMerge pins that a merge which
// commits on GitHub and still fails its caller is not rolled back locally: the
// bump is on main, and reporting it unmerged left main and the checkout
// disagreeing until someone pulled by hand.
func TestEngine_Release_MergeErrorAfterServerSideMerge(t *testing.T) {
	dir, origin, _ := releaseRepo(t, "0.1.0")
	fake := &fakeGitHubPullRequests{
		t: t, origin: origin, states: []string{"clean"}, mergeFailsAfterMerging: true,
	}

	tag, err := landingEngine(t, dir, fake).Release(context.Background())
	require.NoError(t, err, "the merge committed server-side; publish must confirm that, not infer failure")
	require.Equal(t, "v0.1.1", tag)
	require.Equal(t, gitIn(t, origin, "rev-parse", "refs/heads/main"), gitIn(t, origin, "rev-list", "-n1", tag))
}

// TestEngine_Release_UnreadableChecksKeepWaiting pins that a check list the API
// will not serve is not read as "no failures": publish keeps waiting and the
// no release is published even when GitHub calls the pull request clean.
func TestEngine_Release_UnreadableChecksKeepWaiting(t *testing.T) {
	dir, origin, _ := releaseRepo(t, "0.1.0")
	fake := &fakeGitHubPullRequests{
		t: t, origin: origin, states: []string{"blocked", "clean"}, checkRunsStatus: http.StatusInternalServerError,
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fake.afterChecks = cancel
	_, err := landingEngine(t, dir, fake).Release(ctx)
	require.ErrorContains(t, err, "publish budget ran out")
	require.False(t, fake.merged)
	require.Empty(t, gitIn(t, origin, "tag", "-l", "v0.1.1"))
}

func TestEngine_Release_MergeableStatesStillRequireSuccessfulChecks(t *testing.T) {
	for _, state := range []string{"clean", "unstable", "has_hooks"} {
		t.Run(state, func(t *testing.T) {
			dir, origin, _ := releaseRepo(t, "0.1.0")
			head := gitIn(t, origin, "rev-parse", "refs/heads/main")
			fake := &fakeGitHubPullRequests{
				t: t, origin: origin, states: []string{state},
				checkRuns: `{"total_count":1,"check_runs":[{"name":"build","status":"completed","conclusion":"failure"}]}`,
			}
			_, err := landingEngine(t, dir, fake).Release(t.Context())
			require.ErrorContains(t, err, "is red (build)")
			require.False(t, fake.merged)
			require.Equal(t, head, gitIn(t, origin, "rev-parse", "refs/heads/main"))
			require.Empty(t, gitIn(t, origin, "tag", "-l", "v0.1.1"))
		})
	}
}

func TestEngine_Release_MissingOrPendingChecksCannotPublish(t *testing.T) {
	for _, checks := range []string{
		`{"total_count":0,"check_runs":[]}`,
		`{"total_count":1,"check_runs":[{"name":"build","status":"in_progress"}]}`,
		`{"total_count":1,"check_runs":[{"name":"build","status":"completed","conclusion":"skipped"}]}`,
	} {
		t.Run(checks, func(t *testing.T) {
			dir, origin, _ := releaseRepo(t, "0.1.0")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fake := &fakeGitHubPullRequests{
				t: t, origin: origin, states: []string{"clean"},
				checkRuns: checks, afterChecks: cancel,
			}
			_, err := landingEngine(t, dir, fake).Release(ctx)
			require.ErrorContains(t, err, "publish budget ran out")
			require.False(t, fake.merged)
			require.Empty(t, gitIn(t, origin, "tag", "-l", "v0.1.1"))
		})
	}
}

// TestEngine_Release_DirectPushNeverResumes pins that the resume heuristic is
// scoped to landings that can actually strand. ReleaseCodeUnits publishes user
// repos over a direct atomic push, where a `release: vX` subject is just a
// commit message and must not convert a requested bump into re-tagging it.
func TestEngine_Release_DirectPushNeverResumes(t *testing.T) {
	dir, origin, _ := releaseRepo(t, "0.1.0")
	gitIn(t, dir, "commit", "--allow-empty", "-m", "release: v0.1.0")
	gitIn(t, dir, "push", "origin", "main")

	m, err := Detect(dir)
	require.NoError(t, err)
	tag, err := (&Engine{Manifest: m, BumpType: "minor", WorkDir: dir}).Release(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v0.2.0", tag, "a direct push cannot strand, so the requested bump stands")
	require.Equal(t, gitIn(t, origin, "rev-parse", "refs/heads/main"), gitIn(t, origin, "rev-list", "-n1", tag))
}
