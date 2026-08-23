// Package github centralizes construction of an authenticated go-github client
// so every caller shares one token-resolution and endpoint policy instead of
// re-deriving the plumbing.
package github

import (
	"os"
	"os/exec"
	"strings"

	gogithub "github.com/google/go-github/v89/github"
)

// NewClient returns a go-github client authenticated with the resolved token
// when one is available and unauthenticated otherwise. When GITHUB_API_URL is
// set — as it is inside GitHub Actions and against GitHub Enterprise — the
// client is pointed at that endpoint.
func NewClient() (*gogithub.Client, error) {
	var options []gogithub.ClientOptionsFunc
	if token := Token(); token != "" {
		options = append(options, gogithub.WithAuthToken(token))
	}
	if endpoint := strings.TrimSpace(os.Getenv("GITHUB_API_URL")); endpoint != "" {
		options = append(options, gogithub.WithEnterpriseURLs(endpoint, endpoint))
	}
	return gogithub.NewClient(options...)
}

// Token resolves a GitHub token from GITHUB_TOKEN/GH_TOKEN, falling back to the
// gh CLI's stored credential. Without the gh fallback, developer/CI callers run
// unauthenticated (60 requests/hour) on machines that are in fact fully
// authenticated via gh.
func Token() string {
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return t
	}
	if t := strings.TrimSpace(os.Getenv("GH_TOKEN")); t != "" {
		return t
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
