package gh

import (
	"net/http"
	"net/http/httptest"
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
