package github

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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
