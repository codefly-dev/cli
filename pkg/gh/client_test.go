package gh

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOwnerReplacesDotsWithDashes(t *testing.T) {
	for _, tc := range []struct{ publisher, want string }{
		{"codefly.dev", "codefly-dev"},
		{"my.org.dev", "my-org-dev"},
		{"codefly", "codefly"},
	} {
		if got := Owner(tc.publisher); got != tc.want {
			t.Fatalf("Owner(%q) = %q, want %q", tc.publisher, got, tc.want)
		}
	}
}

func TestParseRemote(t *testing.T) {
	for _, tc := range []struct {
		remote      string
		owner, repo string
	}{
		{remote: "git@github.com:codefly-dev/cli.git", owner: "codefly-dev", repo: "cli"},
		{remote: "https://github.com/codefly-dev/cli", owner: "codefly-dev", repo: "cli"},
		{remote: "ssh://github.com/codefly-dev/cli.git", owner: "codefly-dev", repo: "cli"},
		{remote: "ssh://git@github.com/codefly-dev/cli.git", owner: "codefly-dev", repo: "cli"},
		// url.insteadOf rewrites and GitHub Actions checkouts both resolve
		// origin to a URL carrying a credential; it is the same repository.
		{remote: "https://x-access-token:gho_notarealtoken@github.com/codefly-dev/cli.git", owner: "codefly-dev", repo: "cli"},
		{remote: "https://gho_notarealtoken@github.com/codefly-dev/cli.git", owner: "codefly-dev", repo: "cli"},
		// `git remote get-url` output arrives newline-terminated.
		{remote: "https://x-access-token:gho_notarealtoken@github.com/codefly-dev/cli.git\n", owner: "codefly-dev", repo: "cli"},
		{remote: "https://gitlab.com/codefly-dev/cli.git"},
		{remote: "https://x-access-token:gho_notarealtoken@gitlab.com/codefly-dev/cli.git"},
		{remote: "https://github.com.example.com/codefly-dev/cli.git"},
		{remote: "https://github.com/codefly-dev"},
		{remote: "https://github.com/codefly-dev/cli/extra"},
		{remote: "/srv/git/cli.git"},
	} {
		t.Run(tc.remote, func(t *testing.T) {
			owner, repo, err := ParseRemote(tc.remote)
			if tc.owner == "" {
				if err == nil {
					t.Fatalf("ParseRemote(%q) = %s/%s, want an error", tc.remote, owner, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRemote(%q): %v", tc.remote, err)
			}
			if owner != tc.owner || repo != tc.repo {
				t.Fatalf("ParseRemote(%q) = %s/%s, want %s/%s", tc.remote, owner, repo, tc.owner, tc.repo)
			}
		})
	}
}

// TestParseRemoteErrorMasksACredential keeps a live access token out of the
// message: an unparseable rewritten remote is printed to the terminal and
// pasted into bug reports.
func TestParseRemoteErrorMasksACredential(t *testing.T) {
	_, _, err := ParseRemote("https://x-access-token:gho_notarealtoken@gitlab.com/codefly-dev/cli.git\n")
	if err == nil {
		t.Fatal("ParseRemote accepted a non-GitHub remote")
	}
	if strings.Contains(err.Error(), "gho_notarealtoken") {
		t.Fatalf("error leaks the credential: %v", err)
	}
	const want = `unrecognized GitHub remote "https://x-access-token:***@gitlab.com/codefly-dev/cli.git"`
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestNewClientAddsAuthorization(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "secret")
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
	}))
	defer server.Close()

	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	resp, err := client.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != "Bearer secret" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer secret")
	}
}

func TestNewClientUnauthenticated(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("PATH", "") // no `gh` on PATH: force the tokenless path

	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if client == nil {
		t.Fatal("NewClient returned a nil client")
	}
}

func TestTokenPrefersEnv(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "from-github-token")
	t.Setenv("GH_TOKEN", "from-gh-token")
	if got := Token(); got != "from-github-token" {
		t.Fatalf("Token() = %q, want GITHUB_TOKEN to win", got)
	}
}

func TestTokenFallsBackToGHToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "from-gh-token")
	if got := Token(); got != "from-gh-token" {
		t.Fatalf("Token() = %q, want GH_TOKEN fallback", got)
	}
}

func TestTokenEmptyWithoutCredentials(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("PATH", "") // no `gh` on PATH: nothing can supply a token
	if got := Token(); got != "" {
		t.Fatalf("Token() = %q, want empty when no credential source exists", got)
	}
}

func TestNewClientHonorsAPIEndpoint(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("PATH", "")
	t.Setenv("GITHUB_API_URL", "https://ghe.example.com/api/v3")

	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if got := client.BaseURL(); got != "https://ghe.example.com/api/v3/" {
		t.Fatalf("BaseURL = %q, want trailing-slash normalized endpoint", got)
	}
}
