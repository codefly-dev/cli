package publish

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/gh"
	"github.com/google/go-github/v89/github"
)

// dependabotApp posts a check run on every commit for its own dependency
// update job. That run reports whether Dependabot could resolve the manifest —
// an upstream tag it cannot fetch fails it — not whether CI admitted the
// commit, so it is never the verdict a release waits on.
const dependabotApp = "dependabot"

// githubActionsApp renders this fleet's CI. A failing check from any other app
// is the shape that wedged publish before Dependabot's was excluded, so the
// failure names its producer and the next one is diagnosable from the error
// rather than from an API dig.
const githubActionsApp = "github-actions"

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
	// registrationGrace is how long a wait tolerates no CI at all on the
	// commit it watches before concluding none is coming. Zero means
	// checksRegistrationGrace.
	registrationGrace time.Duration
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
	if err := l.reclaimBranch(ctx, e, branch, tag); err != nil {
		return false, err
	}
	// No --force: reclaimBranch has already cleared any leftover, so a
	// rejection here is a branch another publisher pushed since — and it is
	// left exactly as that publisher pushed it.
	if _, err := e.git(ctx, "push", "origin", "HEAD:refs/heads/"+branch); err != nil {
		if now, lsErr := e.git(ctx, "ls-remote", "--heads", "origin", "refs/heads/"+branch); lsErr == nil && strings.TrimSpace(now) != "" {
			return false, errors.Join(anotherPublisher(tag, branch, branch+" appeared on origin while this run was pushing it"), err)
		}
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

// merge waits for the release pull request to become mergeable and merges it,
// through the one merge loop the CLI has (gh.PullRequestMerge). The wait is
// the point: the bump is admitted exactly when the CI every other change
// passes goes green.
//
// Squash with an explicit title keeps main's commit subject identical to what
// a direct push produced — which is also the marker a re-run reads to tell an
// unfinished release from a fresh one.
func (l *pullRequestLanding) merge(ctx context.Context, number int, tag string) error {
	deadline := newRegistrationDeadline(l.registrationGrace)
	merge := &gh.PullRequestMerge{
		Client: l.client, Owner: l.owner, Repo: l.repo, Number: number,
		Noun: "release pull request", Poll: l.poll,
		Ready: func(ctx context.Context, sha string) (bool, error) {
			return l.checksReady(ctx, sha, deadline)
		},
		Method: "squash", CommitTitle: releaseCommitSubject(tag),
		Say: func(step string) { fmt.Printf("==> %s\n", step) },
	}
	_, err := merge.Land(ctx)
	var open *gh.StillOpenError
	if errors.As(err, &open) {
		return fmt.Errorf("release pull request #%d was still %q when the publish budget ran out; it is open and will not be merged by publish", number, open.State)
	}
	return err
}

// checksReady requires positive success, not merely permission to merge.
// An unreadable or absent check list never authorizes release publication.
// A commit on which no CI at all has registered once the deadline's grace is
// spent fails the wait instead of holding it for the whole publish budget.
func (l *pullRequestLanding) checksReady(ctx context.Context, sha string, deadline *registrationDeadline) (bool, error) {
	verdict, err := l.inspectChecks(ctx, sha)
	if err != nil {
		fmt.Printf("==> warning: cannot read checks for %s, still waiting: %v\n", sha, err)
		return false, nil
	}
	if len(verdict.failed) > 0 {
		return false, fmt.Errorf("is red (%s); fix CI and publish again", strings.Join(verdict.failed, ", "))
	}
	if deadline != nil && deadline.expired(verdict.observed) {
		return false, fmt.Errorf("%s after %s: %w", sha, deadline.grace, errNothingRegistered)
	}
	return verdict.ready, nil
}

// checkVerdict is what the checks on one commit say about it.
type checkVerdict struct {
	ready  bool
	failed []string
	// observed reports whether any CI registered on the commit at all —
	// running, finished or failed. Dependabot's own run is not CI.
	observed bool
}

func (l *pullRequestLanding) readChecks(ctx context.Context, sha string) (bool, []string, error) {
	verdict, err := l.inspectChecks(ctx, sha)
	return verdict.ready, verdict.failed, err
}

func (l *pullRequestLanding) inspectChecks(ctx context.Context, sha string) (checkVerdict, error) {
	options := &github.ListCheckRunsOptions{Filter: github.Ptr("latest"), ListOptions: github.ListOptions{PerPage: 100}}
	var failed []string
	pending, succeeded, observed := false, false, false
	for {
		runs, response, err := l.client.Checks.ListCheckRunsForRef(ctx, l.owner, l.repo, sha, options)
		if err != nil {
			return checkVerdict{}, err
		}
		for _, run := range runs.CheckRuns {
			if run.GetApp().GetSlug() == dependabotApp {
				continue
			}
			observed = true
			if run.GetStatus() != runCompleted {
				pending = true
				continue
			}
			switch run.GetConclusion() {
			case checkSuccess:
				succeeded = true
			case "skipped", "neutral":
			default:
				name := run.GetName()
				if slug := run.GetApp().GetSlug(); slug != "" && slug != githubActionsApp {
					name = fmt.Sprintf("%s (%s)", name, slug)
				}
				failed = append(failed, name)
			}
		}
		if response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}
	status, _, err := l.client.Repositories.GetCombinedStatus(ctx, l.owner, l.repo, sha, &github.ListOptions{PerPage: 100})
	if err != nil {
		return checkVerdict{}, err
	}
	if status.GetTotalCount() > 0 {
		observed = true
		switch status.GetState() {
		case checkSuccess:
			succeeded = true
		case "pending":
			pending = true
		default:
			failed = append(failed, "commit status "+status.GetState())
		}
	}
	return checkVerdict{ready: succeeded && !pending && len(failed) == 0, failed: failed, observed: observed}, nil
}

func (l *pullRequestLanding) waitForChecks(ctx context.Context, sha string) error {
	deadline := newRegistrationDeadline(l.registrationGrace)
	for {
		ready, err := l.checksReady(ctx, sha, deadline)
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

// liveBranchGrace is how recent a release branch's head commit must be for
// publish to treat it as another publisher's, even before that publisher has
// opened its pull request. A publisher commits its bump, pushes the branch and
// opens the pull request within seconds; the grace covers that window and a
// few minutes of clock skew between the two machines.
const liveBranchGrace = 5 * time.Minute

// errAnotherPublisher is the refusal for a release branch this run did not
// create and has no claim on. Two publishers bumping the same version used to
// take turns overwriting release-x.y.z, and the loser failed on a 409 about a
// head branch that was out of date, which named neither the cause nor the
// other run.
var errAnotherPublisher = errors.New("another publisher is running")

func anotherPublisher(tag, branch, evidence string) error {
	return fmt.Errorf("%w for %s: %s; wait for it to finish, or, if no publisher is running, close the pull request and delete %s on origin, then publish again",
		errAnotherPublisher, tag, evidence, branch)
}

// reclaimBranch clears a release branch left behind by an earlier attempt at
// this same version, so a retry is not wedged forever behind a non-fast-forward
// rejection whose git error names no remedy.
//
// Release has already established that this version is untagged both locally
// and on origin, and that main carries no untagged release commit for it, so
// an existing release-x.y.z belongs to an attempt that did not complete — or
// to one that has not completed yet. A publisher that is still running keeps
// its pull request open, and has just pushed its branch before opening one;
// either is refused by name rather than deleted from under it. Only a branch
// with neither is a leftover, and deleting it also closes any pull request
// that attempt left behind, which keeps a re-run from stacking a second one.
func (l *pullRequestLanding) reclaimBranch(ctx context.Context, e *Engine, branch, tag string) error {
	out, err := e.git(ctx, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("check for a leftover release branch %s: %w", branch, err)
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}
	open, _, err := l.client.PullRequests.List(ctx, l.owner, l.repo, &github.PullRequestListOptions{
		State: "open", Head: l.owner + ":" + branch,
	})
	if err != nil {
		return fmt.Errorf("check for an open pull request on %s: %w", branch, err)
	}
	if len(open) > 0 {
		return anotherPublisher(tag, branch, fmt.Sprintf("release pull request #%d is open on %s", open[0].GetNumber(), branch))
	}
	if _, err = e.git(ctx, "fetch", "--no-tags", "origin", "refs/heads/"+branch); err != nil {
		return fmt.Errorf("read release branch %s: %w", branch, err)
	}
	committed, err := e.git(ctx, "log", "-1", "--format=%ct", "FETCH_HEAD")
	if err != nil {
		return fmt.Errorf("read release branch %s: %w", branch, err)
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(committed), 10, 64)
	if err != nil {
		return fmt.Errorf("read release branch %s commit time %q: %w", branch, committed, err)
	}
	if age := time.Since(time.Unix(seconds, 0)); age < liveBranchGrace {
		return anotherPublisher(tag, branch, fmt.Sprintf("%s was pushed %s ago and its pull request is not open yet", branch, age.Round(time.Second)))
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
