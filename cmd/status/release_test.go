package status

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/require"
)

// TestCreateAgentIssuePostsToAPI covers the chore-issue migration: the "core
// behind" issue is opened via the API against the agent's origin repository,
// carrying the chore/dependencies labels.
func TestCreateAgentIssuePostsToAPI(t *testing.T) {
	baseDir := t.TempDir()
	agentPath := filepath.Join(baseDir, "web")
	require.NoError(t, os.MkdirAll(agentPath, 0o755))
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", "https://github.com/testowner/testrepo.git"},
	} {
		require.NoError(t, exec.Command("git", append([]string{"-C", agentPath}, args...)...).Run())
	}

	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/repos/testowner/testrepo/issues", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"number":1}`)
	}))
	defer srv.Close()

	original := newGitHubClient
	newGitHubClient = func() (*github.Client, error) {
		endpoint := srv.URL + "/"
		return github.NewClient(github.WithURLs(&endpoint, &endpoint))
	}
	t.Cleanup(func() { newGitHubClient = original })

	require.NoError(t, createAgentIssue(baseDir, AgentStatus{
		Name: "web", CoreVer: "0.1.0", LatestCore: "0.3.0", Delta: 5,
	}))
	require.Contains(t, body["title"], "update core")
	labels, ok := body["labels"].([]any)
	require.True(t, ok, "issue must carry labels")
	require.ElementsMatch(t, []any{"chore", "dependencies"}, labels)
}
