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
		{remote: "https://gho_notarealtoken:x-oauth-basic@github.com/codefly-dev/cli", owner: "codefly-dev", repo: "cli"},
		{remote: "https://user:pass@github.com:443/codefly-dev/cli.git", owner: "codefly-dev", repo: "cli"},
		{remote: "ssh://git@github.com:22/codefly-dev/cli.git", owner: "codefly-dev", repo: "cli"},
		// git accepts a trailing slash and any case in the host.
		{remote: "https://github.com/codefly-dev/cli.git/", owner: "codefly-dev", repo: "cli"},
		{remote: "https://GitHub.com/codefly-dev/cli", owner: "codefly-dev", repo: "cli"},
		// scp-style with another user (GitHub Enterprise Managed Users) or none.
		{remote: "org-123@github.com:codefly-dev/cli.git", owner: "codefly-dev", repo: "cli"},
		{remote: "github.com:codefly-dev/cli", owner: "codefly-dev", repo: "cli"},
		{remote: "https://gitlab.com/codefly-dev/cli.git"},
		{remote: "https://x-access-token:gho_notarealtoken@gitlab.com/codefly-dev/cli.git"},
		{remote: "https://github.com.example.com/codefly-dev/cli.git"},
		{remote: "git@github.com.example.com:codefly-dev/cli.git"},
		{remote: "https://github.com@example.com/codefly-dev/cli.git"},
		{remote: "http://github.com/codefly-dev/cli.git"},
		{remote: "git://github.com/codefly-dev/cli.git"},
		{remote: "https://github.com/codefly-dev/.git"},
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

// TestParseRemoteErrorNeverCarriesACredential keeps a live access token out of
// the message for every shape a credential takes in a remote: an unparseable
// rewritten remote is printed to the terminal and pasted into bug reports.
func TestParseRemoteErrorNeverCarriesACredential(t *testing.T) {
	const token = "gho_notarealtoken"
	for _, tc := range []struct{ remote, want string }{
		{
			remote: "https://x-access-token:" + token + "@gitlab.com/codefly-dev/cli.git\n",
			want:   `unrecognized GitHub remote "https://***@gitlab.com/codefly-dev/cli.git"`,
		},
		{
			// GitHub's legacy form puts the token in the user name.
			remote: "https://" + token + ":x-oauth-basic@gitlab.com/codefly-dev/cli",
			want:   `unrecognized GitHub remote "https://***@gitlab.com/codefly-dev/cli"`,
		},
		{
			remote: "https://" + token + "@github.com/codefly-dev",
			want:   `unrecognized GitHub remote "https://***@github.com/codefly-dev"`,
		},
		{
			// An unencoded '/' in the secret makes the URL unparseable.
			remote: "https://user:a/" + token + "@github.com/codefly-dev/cli",
			want:   `unrecognized GitHub remote "https://***@github.com/codefly-dev/cli"`,
		},
		{
			remote: token + "@gitlab.com:codefly-dev/cli.git",
			want:   `unrecognized GitHub remote "***@gitlab.com:codefly-dev/cli.git"`,
		},
	} {
		t.Run(tc.want, func(t *testing.T) {
			_, _, err := ParseRemote(tc.remote)
			if err == nil {
				t.Fatalf("ParseRemote accepted %q", tc.remote)
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("error leaks the credential: %v", err)
			}
			if err.Error() != tc.want {
				t.Fatalf("error = %q, want %q", err.Error(), tc.want)
			}
		})
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
