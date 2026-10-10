// Package gh provides a shared authenticated go-github client and token
// resolution for the CLI's platform (REST API) flows — agent-release
// publishing and version listing — so they resolve credentials one way.
package gh

import (
	"fmt"
	"net/url"
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
	var options []github.ClientOptionsFunc
	if token := Token(); token != "" {
		options = append(options, github.WithAuthToken(token))
	}
	// GITHUB_API_URL is set by GitHub Actions and against GitHub Enterprise;
	// honoring it points the client at the right host (and gives tests an HTTP seam).
	if endpoint := strings.TrimSpace(os.Getenv("GITHUB_API_URL")); endpoint != "" {
		options = append(options, github.WithEnterpriseURLs(endpoint, endpoint))
	}
	return github.NewClient(options...)
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

// ParseRemote resolves a git remote URL to its GitHub owner and repository.
// Accepts the forms git itself writes for github.com — scp-style
// (git@github.com:owner/repo), https, and ssh:// — with or without the .git
// suffix, and whether or not the URL carries userinfo: a url.insteadOf rewrite
// (and a GitHub Actions checkout) resolves origin to a credential-bearing URL,
// which addresses the same repository. Raw captured command output is fine;
// surrounding whitespace is trimmed here. Anything else is an error rather than
// a guess: the callers use the result to address the API, and a mis-parsed
// remote would address the wrong repository.
func ParseRemote(remote string) (string, string, error) {
	path, ok := githubRepositoryPath(strings.TrimSuffix(strings.TrimSpace(remote), ".git"))
	if !ok {
		return "", "", unrecognizedRemote(remote)
	}
	owner, repo, ok := strings.Cut(path, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", unrecognizedRemote(remote)
	}
	return owner, repo, nil
}

// githubRepositoryPath reduces a github.com remote to its owner/repo path.
func githubRepositoryPath(remote string) (string, bool) {
	if path, ok := strings.CutPrefix(remote, "git@github.com:"); ok {
		return path, true
	}
	parsed, err := url.Parse(remote)
	if err != nil || parsed.Hostname() != "github.com" {
		return "", false
	}
	if parsed.Scheme != "https" && parsed.Scheme != "ssh" {
		return "", false
	}
	return strings.TrimPrefix(parsed.Path, "/"), true
}

// unrecognizedRemote names a remote the parser cannot read, with the secret
// half of any userinfo masked. A url.insteadOf rewrite puts a live access token
// in the resolved URL, and this message is printed to a terminal and pasted
// into bug reports.
func unrecognizedRemote(remote string) error {
	return fmt.Errorf("unrecognized GitHub remote %q", maskUserinfo(strings.TrimSpace(remote)))
}

func maskUserinfo(remote string) string {
	scheme := strings.Index(remote, "://")
	if scheme < 0 {
		return remote
	}
	authority := remote[scheme+3:]
	host := strings.IndexByte(authority, '/')
	if host < 0 {
		host = len(authority)
	}
	at := strings.LastIndexByte(authority[:host], '@')
	if at < 0 {
		return remote
	}
	masked := "***"
	if name, _, hasSecret := strings.Cut(authority[:at], ":"); hasSecret {
		masked = name + ":***"
	}
	return remote[:scheme+3] + masked + authority[at:]
}
