// Package gh provides a shared authenticated go-github client and token
// resolution for the CLI's platform (REST API) flows — agent-release
// publishing, version listing, promotion pull requests, chore issues, and
// library repository creation — so they resolve credentials and owner/repo one
// way. Git *content* operations (clone/add/commit/tag/push) stay on the git
// binary; the sole git touchpoint here is RepoAtDir, which reads the origin
// remote's URL to derive the owner/repo an API call needs — a config read.
package gh

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/google/go-github/v89/github"
)

// Owner maps a codefly publisher to its GitHub repository owner: dots become
// dashes (codefly.dev -> codefly-dev). This is the single source of the rule
// that core's manager.DownloadURL (the install resolver) and the release
// upload/verify paths must all agree on; keeping it in one place is what stops
// an upload target from silently drifting from the URL installers request.
func Owner(publisher string) string {
	return strings.ReplaceAll(publisher, ".", "-")
}

// NewClient returns a client authenticated with a resolved token when one is
// available, or an anonymous client otherwise. Authenticating lifts the
// unauthenticated 60/hour rate limit that turns listing many pinned agents
// flaky, and it is what lets release publishing write to the API at all.
func NewClient() (*github.Client, error) {
	if token := Token(); token != "" {
		return github.NewClient(github.WithAuthToken(token))
	}
	return github.NewClient()
}

// Token resolves a GitHub token from GITHUB_TOKEN/GH_TOKEN, falling back to the
// `gh` CLI's stored credential. The fallback keeps local dev working without
// exporting a token; because it is only a credential source, the `gh` binary is
// optional (present a token via env and it is never invoked) rather than
// required.
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

// RepoAtDir resolves the owner and repository name from the `origin` remote of
// the git working tree at dir — the same repository the `gh` CLI infers when it
// runs from that directory. Platform operations need the owner/repo pair
// explicitly because, unlike `gh`, the API client does not read it from the
// ambient git remote.
func RepoAtDir(ctx context.Context, dir string) (owner, repo string, err error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("resolve origin remote in %s: %w", dir, err)
	}
	return ParseRemote(strings.TrimSpace(string(out)))
}

// ParseRemote extracts the owner and repository name from a github.com remote
// URL in either HTTPS (https://github.com/owner/repo.git) or SSH
// (git@github.com:owner/repo.git) form. A remote on any other host is rejected:
// the derived owner/repo is only meaningful against api.github.com, so silently
// accepting a non-github.com host would send an API call to the wrong place.
func ParseRemote(remote string) (owner, repo string, err error) {
	trimmed := strings.TrimSuffix(remote, ".git")
	trimmed = strings.TrimSuffix(trimmed, "/")
	index := strings.Index(trimmed, "github.com")
	if index < 0 {
		return "", "", fmt.Errorf("not a github.com remote: %q", remote)
	}
	trimmed = trimmed[index+len("github.com"):]
	trimmed = strings.TrimLeft(trimmed, ":/")
	segments := strings.Split(trimmed, "/")
	if len(segments) < 2 || segments[len(segments)-2] == "" || segments[len(segments)-1] == "" {
		return "", "", fmt.Errorf("cannot derive owner/repo from remote %q", remote)
	}
	return segments[len(segments)-2], segments[len(segments)-1], nil
}
