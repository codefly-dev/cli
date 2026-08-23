// Package gh centralizes GitHub API access for the CLI: one authenticated
// client, one token-resolution path, and small platform helpers shared by the
// drift tooling. Keeping this in one place means agent-drift (cmd/agents) and
// the local release scan (cmd/status) resolve credentials and archived state
// identically instead of each re-deriving them.
package gh

import (
	"context"
	"os"
	"os/exec"
	"strings"

	"github.com/google/go-github/v89/github"
)

// Token resolves a GitHub token from GITHUB_TOKEN/GH_TOKEN, falling back to the
// `gh` CLI's stored credential. Without the `gh` fallback, authenticated-only
// diagnostics silently drop to the unauthenticated 60 req/hour limit on a
// machine that is in fact logged in via `gh`.
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

// Client returns a GitHub client authenticated with Token() when one is
// available, and an unauthenticated client otherwise. It returns nil only if
// the underlying client cannot be constructed, which callers should treat as
// "GitHub unavailable" rather than fatal.
func Client() *github.Client {
	var (
		c   *github.Client
		err error
	)
	if token := Token(); token != "" {
		c, err = github.NewClient(github.WithAuthToken(token))
	} else {
		c, err = github.NewClient()
	}
	if err != nil {
		return nil
	}
	return c
}

// Archived reports whether owner/repo is an archived GitHub repository.
// A lookup failure (missing repo, rate limit, no auth, no client) returns false
// so callers only skip a repo on a *confirmed* archived flag and otherwise
// degrade to their normal behavior rather than silently hiding live repos.
func Archived(ctx context.Context, owner, repo string) bool {
	if owner == "" || repo == "" {
		return false
	}
	client := Client()
	if client == nil {
		return false
	}
	r, _, err := client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return false
	}
	return r.GetArchived()
}
