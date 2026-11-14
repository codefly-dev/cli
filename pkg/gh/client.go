// Package gh provides a shared authenticated go-github client and token
// resolution for the CLI's platform (REST API) flows — agent-release
// publishing, version listing and, through core's loader, agent release
// downloads — so they resolve credentials one way.
package gh

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/redact"
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
	if t := environmentToken(); t != "" {
		return t
	}
	token, _ := loginToken("")
	return token
}

// releaseHost is the host every agent release lives on: core's loader reads
// releases from github.com and nothing else, so the login asked is that
// host's, whatever gh's default host is.
const releaseHost = "github.com"

// ReleaseToken is the CLI's manager.ReleaseTokenSource, the credential core's
// agent loader reads a private release with: GITHUB_TOKEN, else GH_TOKEN, else
// the developer's gh login for github.com. The environment comes first, so a
// token exported for one command still wins. The login is read from gh's
// standard output, the token never enters an argument list, and nothing here
// logs it or keeps it beyond the call. With none, it answers every source it
// tried, for the refusal to name.
func ReleaseToken() (string, []string) {
	if t := environmentToken(); t != "" {
		return t, nil
	}
	token, reason := loginToken(releaseHost)
	if token != "" {
		return token, nil
	}
	return "", []string{"GITHUB_TOKEN", "GH_TOKEN", fmt.Sprintf("the gh CLI login for %s (%s)", releaseHost, reason)}
}

func environmentToken() string {
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return t
	}
	return strings.TrimSpace(os.Getenv("GH_TOKEN"))
}

// loginTimeout bounds asking gh for its login: it reads a keychain or a file
// and answers at once, and a gh that hangs must not hang an agent download.
const loginTimeout = 10 * time.Second

// loginToken asks gh for its stored token — for hostname when one is named,
// else for gh's default host — and answers it, or "" and why not. gh's
// standard error is discarded rather than relayed: what it says is gh's, and
// the reason this returns is printed in a refusal.
func loginToken(hostname string) (token, reason string) {
	path, err := exec.LookPath("gh")
	if err != nil {
		return "", "gh is not installed"
	}
	args := []string{"auth", "token"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, path, args...)
	var stdout bytes.Buffer
	command.Stdout = &stdout
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return "", "gh did not answer within " + loginTimeout.String()
		}
		return "", "gh has no login for it; run `gh auth login`"
	}
	if token = strings.TrimSpace(stdout.String()); token == "" {
		return "", "gh answered no token"
	}
	return token, ""
}

// ParseRemote resolves a git remote URL to its GitHub owner and repository.
// Accepts the forms git itself writes for github.com — scp-style
// ([user@]github.com:owner/repo), https, and ssh:// — with or without the .git
// suffix or a trailing slash, and whether or not the URL carries userinfo: a
// url.insteadOf rewrite (and a GitHub Actions checkout) resolves origin to a
// credential-bearing URL, which addresses the same repository. Raw captured
// command output is fine; surrounding whitespace is trimmed here. Anything else
// is an error rather than a guess: the callers use the result to address the
// API, and a mis-parsed remote would address the wrong repository.
//
// The error never carries a credential: the remote it names is redacted.
func ParseRemote(remote string) (string, string, error) {
	path, ok := githubRepositoryPath(strings.TrimSpace(remote))
	if !ok {
		return "", "", unrecognizedRemote(remote)
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	owner, repo, ok := strings.Cut(path, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", unrecognizedRemote(remote)
	}
	return owner, repo, nil
}

// githubRepositoryPath reduces a github.com remote to its path. Host matching is
// exact (case-insensitive, as DNS is): a look-alike such as
// github.com.example.com, or a userinfo that merely names github.com, is not
// GitHub.
func githubRepositoryPath(remote string) (string, bool) {
	if !strings.Contains(remote, "://") {
		// scp-style: [user@]host:path, where the host part holds no '/'.
		authority, path, ok := strings.Cut(remote, ":")
		if !ok || strings.Contains(authority, "/") {
			return "", false
		}
		if at := strings.LastIndexByte(authority, '@'); at >= 0 {
			authority = authority[at+1:]
		}
		return path, strings.EqualFold(authority, "github.com")
	}
	parsed, err := url.Parse(remote)
	if err != nil || !strings.EqualFold(parsed.Hostname(), "github.com") {
		return "", false
	}
	if parsed.Scheme != "https" && parsed.Scheme != "ssh" {
		return "", false
	}
	return parsed.Path, true
}

// unrecognizedRemote names a remote the parser cannot read with its userinfo
// redacted: a url.insteadOf rewrite puts a live access token in the resolved
// URL, and this message is printed to a terminal and pasted into bug reports.
func unrecognizedRemote(remote string) error {
	return fmt.Errorf("unrecognized GitHub remote %q", redact.Remote(remote))
}
