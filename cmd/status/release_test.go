package status

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/go-github/v89/github"
)

func TestParseGitHubRemote(t *testing.T) {
	cases := []struct {
		remote string
		owner  string
		repo   string
		ok     bool
	}{
		{"git@github.com:codefly-dev/service-redis.git", "codefly-dev", "service-redis", true},
		{"https://github.com/codefly-dev/service-redis.git", "codefly-dev", "service-redis", true},
		{"https://github.com/codefly-dev/service-redis", "codefly-dev", "service-redis", true},
		{"git@gitlab.com:codefly-dev/service-redis.git", "", "", false},
		{"https://github.com/codefly-dev", "", "", false},
	}
	for _, tc := range cases {
		owner, repo, err := parseGitHubRemote(tc.remote)
		if tc.ok != (err == nil) {
			t.Fatalf("%s: err = %v, want ok=%v", tc.remote, err, tc.ok)
		}
		if tc.ok && (owner != tc.owner || repo != tc.repo) {
			t.Fatalf("%s: got %s/%s, want %s/%s", tc.remote, owner, repo, tc.owner, tc.repo)
		}
	}
}

func TestCreateAgentIssueCreatesIssueThroughAPI(t *testing.T) {
	baseDir := t.TempDir()
	agentPath := filepath.Join(baseDir, "service-redis")
	if err := exec.Command("git", "init", "-q", agentPath).Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", agentPath, "remote", "add", "origin",
		"git@github.com:codefly-dev/service-redis.git").Run(); err != nil {
		t.Fatal(err)
	}

	var gotPath string
	var payload github.IssueRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"number":7,"html_url":"https://github.com/codefly-dev/service-redis/issues/7"}`))
	}))
	defer server.Close()
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_API_URL", server.URL)

	status := AgentStatus{Name: "service-redis", CoreVer: "0.3.0", LatestCore: "0.3.5", Delta: 5}
	if err := createAgentIssue(baseDir, status); err != nil {
		t.Fatalf("createAgentIssue: %v", err)
	}
	if gotPath != "/api/v3/repos/codefly-dev/service-redis/issues" {
		t.Fatalf("request path = %q", gotPath)
	}
	if payload.Labels == nil || len(*payload.Labels) != 2 {
		t.Fatalf("labels = %v, want [chore dependencies]", payload.Labels)
	}
}
