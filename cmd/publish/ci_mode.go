package publish

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The identity a CI-run release commits and tags as when the runner carries
// none of its own. It is GitHub's documented Actions bot identity, so the
// release branch commit is attributed to automation rather than to whoever
// owns the token. main never carries this commit: the release pull request is
// squash-merged by GitHub, which authors and signs the commit main receives.
const (
	ciGitName  = "github-actions[bot]"
	ciGitEmail = "41898282+github-actions[bot]@users.noreply.github.com"
)

// checksRegistrationGrace bounds how long a release waits for the first check
// run, commit status or tag-triggered workflow run to appear at all. CI
// registers its runs within seconds of the event that starts it, so silence
// for this long means nothing is coming: waiting out the full publish budget
// would only report the same thing forty-five minutes later.
const checksRegistrationGrace = 10 * time.Minute

// errNothingRegistered explains the one way a release run from GitHub Actions
// waits forever: GitHub starts no workflow for an event caused by a workflow's
// own GITHUB_TOKEN, so a release branch pushed, pull request opened or tag
// pushed with that token never gets the CI publish is waiting for.
var errNothingRegistered = errors.New("no CI registered; GitHub starts no workflow for a push, pull request or tag made with a workflow's own GITHUB_TOKEN, " +
	"so a publish running in GitHub Actions must check out and publish with a token that does trigger workflows (a GitHub App installation token or a PAT — " +
	"the `release-token` secret of codefly-dev/cli's publish-agent.yml); outside Actions, check that this repository's CI runs on pull requests and pushes to main")

// isCIEnvironment reports whether the process runs under a CI runner, the way
// every major CI system (GitHub Actions included) announces it.
func isCIEnvironment() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("CI")))
	return value != "" && value != "0" && value != "false" && value != "no"
}

// ciModeFromFlags resolves --ci: explicit when passed, else detected from CI.
func ciModeFromFlags(c *cobra.Command) bool {
	if c.Flags().Changed("ci") {
		on, _ := c.Flags().GetBool("ci")
		return on
	}
	return isCIEnvironment()
}

// prepareCI resolves the git overrides a CI-run release needs, once, before any
// git call. A laptop release inherits the operator's git configuration; a
// runner has none, so without these the release commit fails for want of an
// author, and a repository or global config that asks for signing fails for
// want of a key the runner does not hold.
//
// The overrides are narrow on purpose:
//   - signing is turned off for commits and tags created here. The release
//     commit never reaches main (GitHub squash-merges the pull request and
//     signs what main carries), so the only unsigned object that ships is the
//     annotated tag, which names that GitHub-signed commit.
//   - the identity is supplied only when git has none, so a runner that was
//     configured deliberately keeps its own.
//
// A release that is required to sign its tag (Engine.SignTag) cannot run in CI
// mode: there is no key to sign with, and silently dropping the signature would
// publish a weaker tag than the caller asked for.
func (e *Engine) prepareCI(ctx context.Context) error {
	if !e.CI || e.ciArgs != nil {
		return nil
	}
	if e.SignTag {
		return errors.New("this release requires a signed tag, which CI mode cannot produce: a runner holds no signing key")
	}
	args := []string{"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}
	if !e.gitHasIdentity(ctx) {
		args = append(args, "-c", "user.name="+ciGitName, "-c", "user.email="+ciGitEmail)
	}
	e.ciArgs = args
	return nil
}

// gitHasIdentity reports whether git can already name a committer, from its
// configuration or from the environment variables that override it.
func (e *Engine) gitHasIdentity(ctx context.Context) bool {
	if os.Getenv("GIT_COMMITTER_EMAIL") != "" && os.Getenv("GIT_AUTHOR_EMAIL") != "" {
		return true
	}
	out, err := e.git(ctx, "config", "--get", "user.email")
	return err == nil && strings.TrimSpace(out) != ""
}

// registrationDeadline tracks the grace a wait gives CI to show up at all.
type registrationDeadline struct {
	grace   time.Duration
	started time.Time
}

func newRegistrationDeadline(grace time.Duration) *registrationDeadline {
	if grace <= 0 {
		grace = checksRegistrationGrace
	}
	return &registrationDeadline{grace: grace, started: time.Now()}
}

// expired reports whether nothing has registered for longer than the grace.
func (d *registrationDeadline) expired(observed bool) bool {
	return !observed && time.Since(d.started) > d.grace
}
