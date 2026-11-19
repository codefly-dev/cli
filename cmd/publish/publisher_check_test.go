package publish

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRemoteStrandedReleaseWaitsOnlyForIndependentChecks(t *testing.T) {
	for _, state := range []string{"success", "failure", "pending", "missing-job", "wrong-runner", "missing-run", "wrong-sha", "ambiguous-job"} {
		t.Run(state, func(t *testing.T) {
			dir, origin, _ := releaseRepo(t, "0.1.0")
			gitIn(t, dir, "commit", "--allow-empty", "-m", "release: v0.1.0")
			gitIn(t, dir, "push", "origin", "main")
			sha := gitIn(t, dir, "rev-parse", "HEAD")
			t.Setenv("GITHUB_ACTIONS", "true")
			t.Setenv("GITHUB_REPOSITORY", "codefly-dev/cli")
			t.Setenv("GITHUB_RUN_ID", "42")
			t.Setenv("GITHUB_RUN_ATTEMPT", "1")
			t.Setenv("GITHUB_SHA", sha)
			t.Setenv("RUNNER_NAME", "publisher-runner")
			job := fmt.Sprintf(`{"status":"in_progress","runner_name":"publisher-runner","head_sha":%q,"check_run_url":"https://api.github.com/repos/codefly-dev/cli/check-runs/99"}`, sha)
			jobs := `{"jobs":[` + job + `]}`
			switch state {
			case "missing-job":
				jobs = `{"jobs":[]}`
			case "wrong-runner":
				t.Setenv("RUNNER_NAME", "another-runner")
			case "missing-run":
				t.Setenv("GITHUB_RUN_ID", "")
			case "wrong-sha":
				job = `{"status":"in_progress","runner_name":"publisher-runner","head_sha":"wrong"}`
				jobs = `{"jobs":[` + job + `]}`
			case "ambiguous-job":
				jobs = `{"jobs":[` + job + `,` + job + `]}`
			}
			conclusion, status := "success", "completed"
			if state == "failure" {
				conclusion = "failure"
			}
			if state == "pending" {
				conclusion, status = "", "in_progress"
			}
			fake := &fakeGitHubPullRequests{t: t, origin: origin, publisherJobs: jobs,
				checkRuns: fmt.Sprintf(`{"check_runs":[{"id":99,"name":"publisher","status":"in_progress","app":{"slug":"github-actions"}},{"id":100,"name":"independent","status":%q,"conclusion":%q,"app":{"slug":"github-actions"}}]}`, status, conclusion)}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if state == "pending" {
				fake.afterChecks = cancel
			}
			engine := landingEngine(t, dir, fake)
			if state != "success" && state != "failure" && state != "pending" {
				landing := engine.Landing.(*pullRequestLanding)
				verdict, err := landing.inspectChecks(ctx, sha)
				require.Error(t, err)
				require.False(t, verdict.ready)
				return
			}
			tag, err := engine.Release(ctx)
			if state == "success" {
				require.NoError(t, err)
				require.Equal(t, "v0.1.0", tag)
				require.Equal(t, sha, gitIn(t, origin, "rev-list", "-n1", tag))
			} else {
				require.Error(t, err)
				require.Empty(t, gitIn(t, origin, "tag", "-l", "v0.1.0"))
			}
		})
	}
}
