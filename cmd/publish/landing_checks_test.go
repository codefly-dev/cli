package publish

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/require"
)

func TestReleaseChecksInspectEveryPageAndCommitStatus(t *testing.T) {
	for _, tc := range []struct {
		name, lastCheck, status string
		ready, failed           bool
	}{
		{"success", `{"name":"last","status":"completed","conclusion":"success"}`, `{"total_count":0}`, true, false},
		{"pending", `{"name":"last","status":"in_progress"}`, `{"total_count":0}`, false, false},
		{"failed", `{"name":"last","status":"completed","conclusion":"failure"}`, `{"total_count":0}`, false, true},
		{"cancelled", `{"name":"last","status":"completed","conclusion":"cancelled"}`, `{"total_count":0}`, false, true},
		{"skipped", `{"name":"last","status":"completed","conclusion":"skipped"}`, `{"total_count":0}`, true, false},
		{"status pending", `{"name":"last","status":"completed","conclusion":"success"}`, `{"total_count":1,"state":"pending"}`, false, false},
		{"status failed", `{"name":"last","status":"completed","conclusion":"success"}`, `{"total_count":1,"state":"failure"}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pages := 0
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/owner/repo/commits/head/check-runs", func(w http.ResponseWriter, r *http.Request) {
				pages++
				require.Equal(t, "latest", r.URL.Query().Get("filter"))
				if r.URL.Query().Get("page") == "2" {
					fmt.Fprintf(w, `{"total_count":2,"check_runs":[%s]}`, tc.lastCheck)
					return
				}
				w.Header().Set("Link", fmt.Sprintf("<http://%s%s?page=2>; rel=\"next\"", r.Host, r.URL.Path))
				fmt.Fprint(w, `{"total_count":2,"check_runs":[{"name":"first","status":"completed","conclusion":"success"}]}`)
			})
			mux.HandleFunc("/repos/owner/repo/commits/head/status", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, tc.status)
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			base := server.URL + "/"
			client, err := github.NewClient(github.WithURLs(&base, &base))
			require.NoError(t, err)
			landing := &pullRequestLanding{client: client, owner: "owner", repo: "repo"}
			ready, failed, err := landing.readChecks(t.Context(), "head")
			require.NoError(t, err)
			require.Equal(t, 2, pages)
			require.Equal(t, tc.ready, ready)
			require.Equal(t, tc.failed, len(failed) > 0)
		})
	}
}

func TestResumedReleaseCannotTagFailedMain(t *testing.T) {
	dir, origin, _ := releaseRepo(t, "0.1.0")
	gitIn(t, dir, "commit", "--allow-empty", "-m", "release: v0.1.0")
	gitIn(t, dir, "push", "origin", "main")
	fake := &fakeGitHubPullRequests{
		t: t, origin: origin,
		checkRuns: `{"total_count":1,"check_runs":[{"name":"main-build","status":"completed","conclusion":"failure"}]}`,
	}
	_, err := landingEngine(t, dir, fake).Release(t.Context())
	require.ErrorContains(t, err, "refuse to push tag v0.1.0")
	require.Empty(t, gitIn(t, origin, "tag", "-l", "v0.1.0"))
}

func TestReleaseChecksIgnoreDependabotUpdateJob(t *testing.T) {
	for _, dependabot := range []string{
		`{"name":".github/dependabot.yml","status":"completed","conclusion":"failure","app":{"slug":"dependabot"}}`,
		`{"name":".github/dependabot.yml","status":"in_progress","app":{"slug":"dependabot"}}`,
	} {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/owner/repo/commits/head/check-runs", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"total_count":2,"check_runs":[{"name":"native","status":"completed","conclusion":"success","app":{"slug":"github-actions"}},%s]}`, dependabot)
		})
		mux.HandleFunc("/repos/owner/repo/commits/head/status", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"total_count":0}`)
		})
		server := httptest.NewServer(mux)
		base := server.URL + "/"
		client, err := github.NewClient(github.WithURLs(&base, &base))
		require.NoError(t, err)
		landing := &pullRequestLanding{client: client, owner: "owner", repo: "repo"}

		ready, failed, err := landing.readChecks(t.Context(), "head")
		server.Close()

		require.NoError(t, err)
		require.Empty(t, failed, dependabot)
		require.True(t, ready, dependabot)
	}
}
