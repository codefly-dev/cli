package publish

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/gh"
	"github.com/google/go-github/v89/github"
)

// Landing gets a release commit, and the tag naming it, from the local
// checkout onto origin. It is the only part of the release flow that differs
// between a repo whose main accepts a direct push and one whose main admits
// changes only through a pull request; pre-flight, bump, hooks and rollback
// are shared.
type Landing interface {
	// land publishes the release commit at HEAD, then creates and pushes tag.
	//
	// landed reports whether the release commit reached origin/main. Once it
	// has, the local rollback no longer describes reality: the caller must
	// report the partial release instead of unwinding it.
	land(ctx context.Context, e *Engine, tag string) (landed bool, err error)
}

// directPushLanding pushes main and the tag to origin in one atomic push, so a
// rejected tag cannot leave main advanced to a version nothing tags. It is the
// default, and the only landing that works against a repo we cannot reach
// through the GitHub API.
type directPushLanding struct{}

func (directPushLanding) land(ctx context.Context, e *Engine, tag string) (bool, error) {
	if err := e.gitTag(ctx, tag); err != nil {
		return false, fmt.Errorf("tag: %w", err)
	}
	if _, err := e.git(ctx, "push", "--atomic", "origin", "main", tag); err != nil {
		return false, fmt.Errorf("push main and tag atomically: %w", err)
	}
	return true, nil
}

// pullRequestLanding lands the version bump the way every other change lands:
// on a short-lived branch, through a pull request that has to pass the same
// required checks, merged into main. The tag is then cut from the commit main
// actually carries. Tags are outside branch protection, so pushing the tag
// stays direct.
//
// This is what lets main enforce its checks on every account. A direct push of
// a freshly-created release commit carries no check results, so a protected
// main rejects it unless admins are exempt — and in a fleet where every agent
// pushes as the same admin, exempting admins exempts everyone.
type pullRequestLanding struct {
	client *github.Client
	owner  string
	repo   string
	// poll spaces out the mergeability checks while CI runs. The overall
	// budget is the context's: the caller sizes it to the repo's CI.
	poll time.Duration
}

// newPullRequestLanding resolves the GitHub repository behind origin and
// returns a landing that routes its bumps through a pull request.
func newPullRequestLanding(ctx context.Context, workDir string) (*pullRequestLanding, error) {
	owner, repo, err := originRepository(ctx, workDir)
	if err != nil {
		return nil, err
	}
	if gh.Token() == "" {
		return nil, fmt.Errorf("publishing %s/%s needs a GitHub token to open its release pull request: set GITHUB_TOKEN or run `gh auth login`", owner, repo)
	}
	client, err := gh.NewClient()
	if err != nil {
		return nil, err
	}
	return &pullRequestLanding{client: client, owner: owner, repo: repo, poll: 15 * time.Second}, nil
}

func (l *pullRequestLanding) land(ctx context.Context, e *Engine, tag string) (bool, error) {
	branch := "release-" + strings.TrimPrefix(tag, "v")
	// No --force: an existing branch of this name means another release of the
	// same version is in flight, and clobbering it would strand that one.
	if _, err := e.git(ctx, "push", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return false, fmt.Errorf("push release branch %s: %w", branch, err)
	}
	number, err := l.open(ctx, branch, tag)
	if err != nil {
		return false, errors.Join(err, l.deleteBranch(ctx, e, branch))
	}
	fmt.Printf("==> release pull request #%d opened; waiting for main's required checks\n", number)
	if err := l.merge(ctx, number, tag); err != nil {
		return false, errors.Join(err, l.deleteBranch(ctx, e, branch))
	}

	// Past this point the bump is on main and a local unwind would be a lie.
	if err := e.adoptMergedMain(ctx, tag); err != nil {
		return true, err
	}
	if err := e.gitTag(ctx, tag); err != nil {
		return true, fmt.Errorf("tag: %w", err)
	}
	if err := e.pushTag(ctx, tag); err != nil {
		return true, err
	}
	if err := l.deleteBranch(ctx, e, branch); err != nil {
		fmt.Printf("==> warning: %v\n", err)
	}
	return true, nil
}

func (l *pullRequestLanding) open(ctx context.Context, branch, tag string) (int, error) {
	pr, _, err := l.client.PullRequests.Create(ctx, l.owner, l.repo, &github.NewPullRequest{
		Title: github.Ptr(releaseCommitSubject(tag)),
		Head:  github.Ptr(branch),
		Base:  github.Ptr("main"),
		Body: github.Ptr("Version bump for " + tag + ", opened by `codefly publish`.\n\n" +
			"The tag is cut from the commit this merges into `main`, so the release ships exactly what main's required checks admitted."),
	})
	if err != nil {
		return 0, fmt.Errorf("open release pull request for %s: %w", tag, err)
	}
	return pr.GetNumber(), nil
}

// merge waits for the release pull request to become mergeable and merges it.
// The wait is the point: the bump is admitted exactly when the CI every other
// change passes goes green.
//
// Squash with an explicit title keeps main's commit subject identical to what
// a direct push produced — which is also the marker a re-run reads to tell an
// unfinished release from a fresh one.
func (l *pullRequestLanding) merge(ctx context.Context, number int, tag string) error {
	for {
		pr, _, err := l.client.PullRequests.Get(ctx, l.owner, l.repo, number)
		if err != nil {
			return fmt.Errorf("read release pull request #%d: %w", number, err)
		}
		if pr.GetMerged() {
			return nil
		}
		state := pr.GetMergeableState()
		switch state {
		case "clean", "unstable", "has_hooks":
			result, _, err := l.client.PullRequests.Merge(ctx, l.owner, l.repo, number, "", &github.PullRequestOptions{
				MergeMethod: "squash",
				CommitTitle: releaseCommitSubject(tag),
				SHA:         pr.GetHead().GetSHA(),
			})
			if err != nil {
				return fmt.Errorf("merge release pull request #%d: %w", number, err)
			}
			if !result.GetMerged() {
				return fmt.Errorf("merge release pull request #%d: %s", number, result.GetMessage())
			}
			return nil
		case "dirty":
			return fmt.Errorf("release pull request #%d conflicts with main", number)
		case "behind":
			// main is protected with strict checks, so a branch that fell
			// behind can never go green until it is caught up.
			var accepted *github.AcceptedError
			if _, _, err := l.client.PullRequests.UpdateBranch(ctx, l.owner, l.repo, number, nil); err != nil && !errors.As(err, &accepted) {
				return fmt.Errorf("update release pull request #%d onto main: %w", number, err)
			}
		case "blocked":
			if failed := l.failedChecks(ctx, pr.GetHead().GetSHA()); len(failed) > 0 {
				return fmt.Errorf("release pull request #%d is red (%s); fix main's CI and publish again",
					number, strings.Join(failed, ", "))
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("release pull request #%d was still %q when the publish budget ran out; it is open and will not be merged by publish", number, state)
		case <-time.After(l.poll):
		}
	}
}

// failedChecks names the check runs on sha that have concluded badly, so a red
// release pull request fails at once instead of burning the whole wait budget.
func (l *pullRequestLanding) failedChecks(ctx context.Context, sha string) []string {
	runs, _, err := l.client.Checks.ListCheckRunsForRef(ctx, l.owner, l.repo, sha, nil)
	if err != nil {
		return nil
	}
	var failed []string
	for _, run := range runs.CheckRuns {
		switch run.GetConclusion() {
		case "failure", "timed_out", "action_required", "stale":
			failed = append(failed, run.GetName())
		}
	}
	return failed
}

func (l *pullRequestLanding) deleteBranch(ctx context.Context, e *Engine, branch string) error {
	if _, err := e.git(ctx, "push", "origin", "--delete", branch); err != nil {
		return fmt.Errorf("delete release branch %s: %w", branch, err)
	}
	return nil
}

func originRepository(ctx context.Context, workDir string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", "remote", "get-url", "origin")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("resolve origin remote of %s: %w", workDir, err)
	}
	remote := strings.TrimSuffix(strings.TrimSpace(string(out)), ".git")
	switch {
	case strings.HasPrefix(remote, "git@github.com:"):
		remote = strings.TrimPrefix(remote, "git@github.com:")
	case strings.HasPrefix(remote, "https://github.com/"):
		remote = strings.TrimPrefix(remote, "https://github.com/")
	default:
		return "", "", fmt.Errorf("origin %q is not a GitHub repository; publish lands its version bump through a GitHub pull request", remote)
	}
	owner, repo, ok := strings.Cut(remote, "/")
	if !ok || owner == "" || repo == "" {
		return "", "", fmt.Errorf("origin %q is not a GitHub repository; publish lands its version bump through a GitHub pull request", remote)
	}
	return owner, repo, nil
}
