// Package gh provides a shared authenticated go-github client and token
// resolution for the CLI's platform (REST API) flows — agent-release
// publishing and version listing — so they resolve credentials one way.
package gh

import (
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
