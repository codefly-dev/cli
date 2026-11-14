// Package gh provides a shared authenticated go-github client and token
// resolution for the CLI's platform (REST API) flows — agent-release
// publishing, version listing and, through core's loader, agent release
// downloads — so they resolve credentials one way.
package gh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
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
	if t, _ := environmentToken(); t != "" {
		return t
	}
	token, _ := loginToken(context.Background(), "")
	return token
}

// releaseHost is the host every agent release lives on: core's loader reads
// releases from github.com and nothing else, so the login asked is that
// host's, whatever gh's default host is.
const releaseHost = "github.com"

// releaseAPI is the API core's loader reads releases from; a gh login is
// checked against it before it is handed over. A variable for tests only.
var releaseAPI = "https://api.github.com"

// ReleaseTokenSource is the CLI's manager.ReleaseTokenSource, the credential
// core's agent loader reads a private release with: GITHUB_TOKEN, else
// GH_TOKEN, else the developer's gh login for github.com.
//
//   - The environment is read on every call and comes first, so a token
//     exported for one command still wins and gh is never asked.
//   - The login is asked once per source — so once per command, however many
//     agents it downloads — under ctx, so whatever ends the command ends the
//     lookup; a cancelled lookup is not remembered.
//   - A SIGINT or SIGTERM while gh is asked ends the lookup (interruptibly),
//     and changes no one else's signal handling: when signalHandled says the
//     command is not handling signals itself, the signal is handed back to the
//     process's default action once the lookup has stopped listening, so a
//     command that never reads its context still ends on the first Ctrl-C.
//   - A login GitHub refuses is not handed over: `gh auth token` prints a
//     stored token without checking it, and a revoked one would turn every
//     public download into a 401. The loader reads anonymously instead.
//   - The token is read from gh's standard output, never enters an argument
//     list, a log or a file, and lives only in this closure.
//
// With a token, it answers the one source the token came from, so a refusal
// of that token can say whose it was; with none, every source it tried, for
// the refusal to name.
func ReleaseTokenSource(ctx context.Context, signalHandled func() bool) func() (string, []string) {
	if ctx == nil {
		ctx = context.Background()
	}
	var (
		mu     sync.Mutex
		asked  bool
		token  string
		reason string
	)
	return func() (string, []string) {
		if t, source := environmentToken(); t != "" {
			return t, []string{source}
		}
		mu.Lock()
		defer mu.Unlock()
		if !asked {
			var login, why string
			interrupted := interruptibly(ctx, signalHandled, func(ctx context.Context) {
				login, why = releaseLogin(ctx)
			})
			if interrupted || ctx.Err() != nil {
				return "", triedSources("the lookup was cancelled")
			}
			asked, token, reason = true, login, why
		}
		if token != "" {
			return token, []string{loginSource}
		}
		return "", triedSources(reason)
	}
}

// loginSource names the gh login as the source of a token it answered.
const loginSource = "the gh login (" + releaseHost + ")"

func triedSources(loginReason string) []string {
	return []string{"GITHUB_TOKEN", "GH_TOKEN", fmt.Sprintf("the gh CLI login for %s (%s)", releaseHost, loginReason)}
}

// environmentToken is the environment's token and the variable it came from.
func environmentToken() (token, source string) {
	for _, variable := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if t := strings.TrimSpace(os.Getenv(variable)); t != "" {
			return t, variable
		}
	}
	return "", ""
}

// interruptibly runs lookup under ctx, cancelled as well by a SIGINT or
// SIGTERM that arrives while it runs, and answers whether one did. It listens
// only while lookup runs. A signal it caught is then handed back to the
// process (raised again) unless signalHandled says a handler of the command's
// own is listening — that handler received the same signal, and a second one
// would read as "force quit".
func interruptibly(ctx context.Context, signalHandled func() bool, lookup func(context.Context)) bool {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(ctx)
	var caught os.Signal
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case caught = <-signals:
			cancel()
		case <-ctx.Done():
		}
	}()
	lookup(ctx)
	signal.Stop(signals)
	cancel()
	<-watched
	if caught == nil {
		select {
		case caught = <-signals: // arrived as the lookup finished
		default:
			return false
		}
	}
	if signalHandled == nil || !signalHandled() {
		if process, err := os.FindProcess(os.Getpid()); err == nil {
			_ = process.Signal(caught)
		}
	}
	return true
}

// releaseLogin is gh's login for github.com, held to GitHub's answer: a
// token the API refuses (401) is no credential. Any other answer — a network
// failure included — leaves the token to the download, which reports its own.
func releaseLogin(ctx context.Context) (string, string) {
	token, reason := loginToken(ctx, releaseHost)
	if token == "" {
		return "", reason
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseAPI+"/rate_limit", nil)
	if err != nil {
		return token, ""
	}
	request.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: loginTimeout}
	response, err := client.Do(request)
	if err != nil {
		return token, ""
	}
	_ = response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return "", "GitHub refused the login gh holds (401); run `gh auth login` again"
	}
	return token, ""
}

// loginTimeout bounds asking gh for its login: it reads a keychain or a file
// and answers at once, and a gh that hangs must not hang an agent download.
// loginWaitDelay bounds the wait for gh's output to close after it exits or
// is killed, so a child gh leaves holding the pipe cannot outlast the bound.
const (
	loginTimeout   = 10 * time.Second
	loginWaitDelay = time.Second
)

// loginToken asks gh for its stored token — for hostname when one is named,
// else for gh's default host — and answers it, or "" and why not. gh's
// standard error is read only to tell "not logged in" from another failure;
// it is never relayed, because the reason this returns is printed.
func loginToken(ctx context.Context, hostname string) (token, reason string) {
	path, err := exec.LookPath("gh")
	if err != nil {
		return "", "gh is not installed"
	}
	args := []string{"auth", "token"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, path, args...)
	command.WaitDelay = loginWaitDelay
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return "", "gh did not answer within " + loginTimeout.String()
		case ctx.Err() != nil:
			return "", "the lookup was cancelled"
		case notLoggedIn(stderr.String()):
			return "", "gh is not logged in to it; run `gh auth login`"
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", fmt.Sprintf("gh could not answer a token for it (exit status %d)", exit.ExitCode())
		}
		return "", "gh could not be run"
	}
	if token = strings.TrimSpace(stdout.String()); token == "" {
		return "", "gh answered no token"
	}
	return token, ""
}

// notLoggedIn recognises gh's own words for a host it holds no login for:
// "no oauth token found for <host>", and "not logged in" in older releases.
func notLoggedIn(stderr string) bool {
	stderr = strings.ToLower(stderr)
	return strings.Contains(stderr, "no oauth token") || strings.Contains(stderr, "not logged in")
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
