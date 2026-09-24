package publish

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/require"
)

func TestIsCIEnvironment(t *testing.T) {
	for value, want := range map[string]bool{
		"": false, "0": false, "false": false, "FALSE": false, "no": false,
		"true": true, "1": true, "yes": true,
	} {
		t.Setenv("CI", value)
		require.Equal(t, want, isCIEnvironment(), "CI=%q", value)
	}
}

// runnerLikeRepo stands up a release repository the way a CI runner sees it:
// no global git configuration, no identity, and a repository configuration
// that asks for signing with a key nobody holds.
func runnerLikeRepo(t *testing.T) (dir, origin string) {
	t.Helper()
	dir, origin, _ = releaseRepo(t, "0.1.0")

	global := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(global, nil, 0o600))
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "EMAIL"} {
		t.Setenv(name, "")
		require.NoError(t, os.Unsetenv(name))
	}
	gitIn(t, dir, "config", "--unset", "user.name")
	gitIn(t, dir, "config", "--unset", "user.email")
	// user.useConfigOnly stops git inventing an identity from the hostname,
	// which some runners would let it do — the test must not depend on that.
	gitIn(t, dir, "config", "user.useConfigOnly", "true")
	gitIn(t, dir, "config", "commit.gpgsign", "true")
	gitIn(t, dir, "config", "tag.gpgsign", "true")
	gitIn(t, dir, "config", "gpg.program", "false")
	return dir, origin
}

func TestEngine_Release_CIModeReleasesOnARunnerWithNoIdentityOrKey(t *testing.T) {
	dir, origin := runnerLikeRepo(t)
	m, err := Detect(dir)
	require.NoError(t, err)

	tag, err := (&Engine{Manifest: m, BumpType: "patch", WorkDir: dir, CI: true}).Release(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v0.1.1", tag)

	tagger := gitIn(t, origin, "for-each-ref", "--format=%(taggername) %(taggeremail)", "refs/tags/v0.1.1")
	require.Equal(t, ciGitName+" <"+ciGitEmail+">", tagger, "the tag is made as the Actions bot when git has no identity")
	require.Equal(t, "tag", gitIn(t, origin, "cat-file", "-t", "v0.1.1"), "the tag stays annotated")
	require.NotContains(t, gitIn(t, origin, "cat-file", "-p", "v0.1.1"), "BEGIN PGP", "CI mode does not sign")

	// The overrides are per invocation: nothing was written to the checkout.
	_, configErr := (&Engine{WorkDir: dir}).git(context.Background(), "config", "--get", "user.email")
	require.Error(t, configErr, "CI mode must not persist an identity into the repository")
}

func TestEngine_Release_WithoutCIModeARunnerCannotRelease(t *testing.T) {
	dir, origin := runnerLikeRepo(t)
	m, err := Detect(dir)
	require.NoError(t, err)

	_, err = (&Engine{Manifest: m, BumpType: "patch", WorkDir: dir}).Release(context.Background())
	require.Error(t, err, "this is the laptop-only assumption CI mode removes")
	require.Empty(t, gitIn(t, origin, "tag", "-l", "v0.1.1"))
}

func TestEngine_CIModeKeepsAConfiguredIdentity(t *testing.T) {
	dir, _, _ := releaseRepo(t, "0.1.0")
	engine := &Engine{WorkDir: dir, CI: true}
	require.NoError(t, engine.prepareCI(context.Background()))
	require.Equal(t, []string{"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, engine.ciArgs)
}

func TestEngine_CIModeRefusesARequiredSignature(t *testing.T) {
	engine := &Engine{WorkDir: t.TempDir(), CI: true, SignTag: true}
	require.ErrorContains(t, engine.prepareCI(context.Background()), "signed tag")
}

// A release pull request opened with a workflow's own GITHUB_TOKEN never gets
// CI. The wait must say so after its grace, not after the whole budget.
func TestEngine_Release_PullRequestThatNeverGetsCIFailsFast(t *testing.T) {
	dir, origin, manifest := releaseRepo(t, "0.1.0")
	fake := &fakeGitHubPullRequests{
		t: t, origin: origin, states: []string{"clean"},
		checkRuns: `{"total_count":1,"check_runs":[{"name":"Dependabot","status":"completed","conclusion":"success","app":{"slug":"dependabot"}}]}`,
	}
	engine := landingEngine(t, dir, fake)
	engine.Landing.(*pullRequestLanding).registrationGrace = 20 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := engine.Release(ctx)
	require.ErrorIs(t, err, errNothingRegistered)
	require.ErrorContains(t, err, "GITHUB_TOKEN")
	require.NoError(t, ctx.Err(), "the wait must end on the grace, not the budget")

	contents, readErr := os.ReadFile(manifest)
	require.NoError(t, readErr)
	require.Equal(t, "version: 0.1.0\n", string(contents))
	require.Empty(t, gitIn(t, origin, "tag", "-l", "v0.1.1"))
	require.Empty(t, gitIn(t, origin, "branch", "--list", "release-0.1.1"))
}

func TestRegistrationDeadlineOnlyExpiresOnSilence(t *testing.T) {
	deadline := &registrationDeadline{grace: time.Millisecond, started: time.Now().Add(-time.Second)}
	require.True(t, deadline.expired(false))
	require.False(t, deadline.expired(true), "CI that registered and is still running is waited on")
	require.False(t, newRegistrationDeadline(0).expired(false), "zero grace means the production default")
}

func TestWorkflowReleaseStateTellsNotStartedFromRunning(t *testing.T) {
	run := func(status, conclusion string) *github.WorkflowRun {
		return &github.WorkflowRun{
			ID: github.Ptr(int64(1)), HeadSHA: github.Ptr("abc"), HeadBranch: github.Ptr("v1.0.0"),
			Event: github.Ptr("push"), Status: github.Ptr(status), Conclusion: github.Ptr(conclusion),
		}
	}
	ready, started, err := workflowReleaseState(nil, "abc", "v1.0.0")
	require.NoError(t, err)
	require.False(t, ready)
	require.False(t, started)

	ready, started, err = workflowReleaseState([]*github.WorkflowRun{run("in_progress", "")}, "abc", "v1.0.0")
	require.NoError(t, err)
	require.False(t, ready)
	require.True(t, started)

	ready, started, err = workflowReleaseState([]*github.WorkflowRun{run("completed", "success")}, "abc", "v1.0.0")
	require.NoError(t, err)
	require.True(t, ready)
	require.True(t, started)
}

func TestWorkflowOwnedReleaseDoesNotNeedEveryLoaderPlatformOnTheHost(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	previous := hostPlatform
	hostPlatform = func() (string, string) { return "linux", "amd64" } // a GitHub-hosted runner
	t.Cleanup(func() { hostPlatform = previous })

	require.ErrorContains(t, checkAgentReleasePreconditions(agentPublication{Owner: publicationCLI}), "darwin/arm64",
		"when publish uploads the archives itself, the host must still build every platform")
	require.NoError(t, checkAgentReleasePreconditions(agentPublication{Owner: publicationWorkflow, Workflow: "releaser.yml"}),
		"a workflow-owned release builds its platforms in that workflow, so a linux/amd64 runner can publish it")

	reg := serviceRegistration(t)
	assets := expectedLoaderAssets(reg, "go-grpc", "0.1.48")
	require.Len(t, assets, len(loaderPlatforms))
	for i, p := range loaderPlatforms {
		require.Equal(t, p, assets[i].platform)
		require.Equal(t, loaderArchiveName(reg, "go-grpc", "0.1.48", p), filepath.Base(assets[i].archivePath))
		require.Empty(t, assets[i].sbomPath)
	}
}

// fakeActions serves the two Actions endpoints --remote uses.
type fakeActions struct {
	conclusion  string
	workflowNot bool
	dispatched  map[string]any
}

func (f *fakeActions) client(t *testing.T) *github.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/service-example/actions/workflows/publish.yml", func(w http.ResponseWriter, _ *http.Request) {
		if f.workflowNot {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"id":1,"path":".github/workflows/publish.yml"}`)
	})
	mux.HandleFunc("/repos/acme/service-example/actions/workflows/publish.yml/dispatches", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&f.dispatched))
		fmt.Fprint(w, `{"workflow_run_id":42,"html_url":"https://github.com/acme/service-example/actions/runs/42"}`)
	})
	mux.HandleFunc("/repos/acme/service-example/actions/runs/42", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"id":42,"status":"completed","conclusion":%q,"html_url":"https://github.com/acme/service-example/actions/runs/42"}`, f.conclusion)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	base := ts.URL + "/"
	client, err := github.NewClient(github.WithURLs(&base, &base))
	require.NoError(t, err)
	return client
}

func TestRemoteReleaseDispatchesOnMainAndFollowsTheRun(t *testing.T) {
	fake := &fakeActions{conclusion: "success"}
	remote := &remoteRelease{client: fake.client(t), owner: "acme", repo: "service-example", workflow: "publish.yml", poll: time.Millisecond}

	require.NoError(t, remote.check(t.Context()))
	runID, url, err := remote.dispatch(t.Context(), "minor")
	require.NoError(t, err)
	require.Equal(t, int64(42), runID)
	require.True(t, strings.HasSuffix(url, "/runs/42"))
	require.Equal(t, "main", fake.dispatched["ref"], "a release is only ever dispatched on main")
	require.Equal(t, map[string]any{"bump": "minor"}, fake.dispatched["inputs"])
	require.Equal(t, true, fake.dispatched["return_run_details"], "the run is followed by id, never guessed from a listing")
	require.NoError(t, remote.wait(t.Context(), runID))
}

func TestRemoteReleaseReportsAFailedRun(t *testing.T) {
	fake := &fakeActions{conclusion: "failure"}
	remote := &remoteRelease{client: fake.client(t), owner: "acme", repo: "service-example", workflow: "publish.yml", poll: time.Millisecond}
	err := remote.wait(t.Context(), 42)
	require.ErrorContains(t, err, "concluded failure")
	require.ErrorContains(t, err, "/runs/42")
}

func TestRemoteReleaseRefusesARepositoryWithoutTheWorkflow(t *testing.T) {
	fake := &fakeActions{workflowNot: true}
	remote := &remoteRelease{client: fake.client(t), owner: "acme", repo: "service-example", workflow: "publish.yml"}
	require.ErrorContains(t, remote.check(t.Context()), "has no .github/workflows/publish.yml")
}

func TestValidateRemoteBump(t *testing.T) {
	for _, bump := range []string{"patch", "minor", "major"} {
		require.NoError(t, validateRemoteBump(bump))
	}
	for _, bump := range []string{"beta", "", "patch; rm -rf /"} {
		require.Error(t, validateRemoteBump(bump))
	}
}
