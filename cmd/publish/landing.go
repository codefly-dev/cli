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

	// stranding reports whether this landing can leave the bump on main with
	// no tag naming it. Only a landing that can produce that state may have a
	// re-run finish an untagged release commit instead of bumping past it.
	stranding() bool
}

// directPushLanding pushes main and the tag to origin in one atomic push, so a
// rejected tag cannot leave main advanced to a version nothing tags. It is the
// default, and the only landing that works against a repo we cannot reach
// through the GitHub API.
type directPushLanding struct{}

// stranding is false: main and the tag go up in one atomic push, so the remote
// never holds one without the other.
func (directPushLanding) stranding() bool { return false }

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

// stranding is true: the merge and the tag push are separate operations, and a
// failure between them leaves a bumped manifest on main with no release.
func (*pullRequestLanding) stranding() bool { return true }

func (l *pullRequestLanding) land(ctx context.Context, e *Engine, tag string) (bool, error) {
	branch := "release-" + strings.TrimPrefix(tag, "v")
	if err := l.reclaimBranch(ctx, e, branch); err != nil {
		return false, err
	}
	// No --force: reclaimBranch has already cleared any leftover, so a
	// rejection here is a branch this release has no claim on.
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
			ready, checksErr := l.checksReady(ctx, pr.GetHead().GetSHA())
			if checksErr != nil {
				return fmt.Errorf("release pull request #%d: %w", number, checksErr)
			}
			if !ready {
				break
			}
			result, _, err := l.client.PullRequests.Merge(ctx, l.owner, l.repo, number, "", &github.PullRequestOptions{
				MergeMethod: "squash",
				CommitTitle: releaseCommitSubject(tag),
				SHA:         pr.GetHead().GetSHA(),
			})
			if err != nil {
				// The merge can commit server-side and still fail the caller —
				// a dropped response or a 502 after the fact. Reporting that as
				// unmerged rolls the bump back locally while main keeps it, so
				// ask GitHub what actually happened instead of inferring it
				// from the transport.
				if l.confirmMerged(ctx, number) {
					return nil
				}
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
			if _, checksErr := l.checksReady(ctx, pr.GetHead().GetSHA()); checksErr != nil {
				return fmt.Errorf("release pull request #%d: %w", number, checksErr)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("release pull request #%d was still %q when the publish budget ran out; it is open and will not be merged by publish", number, state)
		case <-time.After(l.poll):
		}
	}
}

// checksReady requires positive success, not merely permission to merge.
// An unreadable or absent check list never authorizes release publication.
func (l *pullRequestLanding) checksReady(ctx context.Context, sha string) (bool, error) {
	ready, failed, err := l.readChecks(ctx, sha)
	if err != nil {
		fmt.Printf("==> warning: cannot read checks for %s, still waiting: %v\n", sha, err)
		return false, nil
	}
	if len(failed) > 0 {
		return false, fmt.Errorf("is red (%s); fix CI and publish again", strings.Join(failed, ", "))
	}
	return ready, nil
}

func (l *pullRequestLanding) readChecks(ctx context.Context, sha string) (bool, []string, error) {
	options := &github.ListCheckRunsOptions{Filter: github.Ptr("latest"), ListOptions: github.ListOptions{PerPage: 100}}
	var failed []string
	pending, succeeded := false, false
	for {
		runs, response, err := l.client.Checks.ListCheckRunsForRef(ctx, l.owner, l.repo, sha, options)
		if err != nil {
			return false, nil, err
		}
		for _, run := range runs.CheckRuns {
			if run.GetStatus() != "completed" {
				pending = true
				continue
			}
			switch run.GetConclusion() {
			case checkSuccess:
				succeeded = true
			case "skipped", "neutral":
			default:
				failed = append(failed, run.GetName())
			}
		}
		if response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}
	status, _, err := l.client.Repositories.GetCombinedStatus(ctx, l.owner, l.repo, sha, &github.ListOptions{PerPage: 100})
	if err != nil {
		return false, nil, err
	}
	if status.GetTotalCount() > 0 {
		switch status.GetState() {
		case checkSuccess:
			succeeded = true
		case "pending":
			pending = true
		default:
			failed = append(failed, "commit status "+status.GetState())
		}
	}
	return succeeded && !pending && len(failed) == 0, failed, nil
}

func (l *pullRequestLanding) waitForChecks(ctx context.Context, sha string) error {
	for {
		ready, err := l.checksReady(ctx, sha)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("commit %s has no completed successful CI: %w", sha, ctx.Err())
		case <-time.After(l.poll):
		}
	}
}

// confirmMerged asks whether the pull request is merged, on a context detached
// from the caller's budget so an expired deadline cannot turn a merge that
// succeeded into one reported as failed.
func (l *pullRequestLanding) confirmMerged(ctx context.Context, number int) bool {
	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	pr, _, err := l.client.PullRequests.Get(ctx, l.owner, l.repo, number)
	return err == nil && pr.GetMerged()
}

// reclaimBranch clears a release branch left behind by an earlier attempt at
// this same version, so a retry is not wedged forever behind a non-fast-forward
// rejection whose git error names no remedy.
//
// Reclaiming is unambiguous rather than merely convenient: Release has already
// established that this version is untagged both locally and on origin, and
// that main carries no untagged release commit for it. An existing
// release-x.y.z therefore belongs to an attempt that did not complete. Deleting
// it also closes the stale pull request that attempt left open, which is what
// keeps a re-run from stacking a second one.
func (l *pullRequestLanding) reclaimBranch(ctx context.Context, e *Engine, branch string) error {
	out, err := e.git(ctx, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("check for a leftover release branch %s: %w", branch, err)
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}
	fmt.Printf("==> reclaiming %s, left on origin by an earlier attempt at this version\n", branch)
	return l.deleteBranch(ctx, e, branch)
}

func (l *pullRequestLanding) deleteBranch(ctx context.Context, e *Engine, branch string) error {
	ctx, cancel := cleanupContext(ctx)
	defer cancel()
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
	owner, repo, err := gh.ParseRemote(string(out))
	if err != nil {
		return "", "", fmt.Errorf("%w; publish lands its version bump through a GitHub pull request", err)
	}
	return owner, repo, nil
}
