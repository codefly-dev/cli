package librarystore

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/require"
)

// withTestClient points the package's go-github client seam at a local server.
func withTestClient(t *testing.T, baseURL string) {
	t.Helper()
	original := newGitHubClient
	newGitHubClient = func() (*github.Client, error) {
		endpoint := baseURL + "/"
		return github.NewClient(github.WithURLs(&endpoint, &endpoint))
	}
	t.Cleanup(func() { newGitHubClient = original })
}

// TestGitHubStorePublishDoesNotTouchAPIWhenRepoExists is the regression guard
// for the clone-first ordering: a republish of an existing repository is pure
// git and must never reach the GitHub API. The pre-check-before-clone shape
// this replaced called ensureRepo on every publish, so this test would fail
// there (t.Fatal fires).
func TestGitHubStorePublishDoesNotTouchAPIWhenRepoExists(t *testing.T) {
	ctx := context.Background()
	remote := bareRepo(t)
	s := NewGitHubStore("codefly-dev")
	s.remoteFor = func(Language, string) string { return remote }
	s.ensureRepo = func(context.Context, Language, string) error {
		t.Fatal("ensureRepo must not run when the repository already exists")
		return nil
	}
	modulePath := goModulePath(remote)
	_, err := s.Publish(ctx, goModule(t, modulePath, "package authkit\n\nconst V = 1\n"),
		Coordinates{Language: LanguageGo, Name: "authkit", Version: "1.0.0"})
	require.NoError(t, err)
}

// TestGitHubStorePublishCreatesMissingRepository proves the recovery path:
// the first clone fails (remote does not exist yet), ensureRepo creates it,
// and the retry clones into a clean directory and publishes.
func TestGitHubStorePublishCreatesMissingRepository(t *testing.T) {
	ctx := context.Background()
	remote := filepath.Join(t.TempDir(), "remote.git") // does not exist yet
	s := NewGitHubStore("codefly-dev")
	s.remoteFor = func(Language, string) string { return remote }
	created := 0
	s.ensureRepo = func(context.Context, Language, string) error {
		created++
		return exec.Command("git", "init", "--quiet", "--bare", remote).Run()
	}
	modulePath := goModulePath(remote)
	published, err := s.Publish(ctx, goModule(t, modulePath, "package authkit\n\nconst V = 1\n"),
		Coordinates{Language: LanguageGo, Name: "authkit", Version: "1.0.0"})
	require.NoError(t, err)
	require.Equal(t, 1, created, "ensureRepo must run exactly once, only after the first clone fails")
	require.Equal(t, modulePath, published.ImportPath)
}

func TestCreateRepositoryIfMissing(t *testing.T) {
	t.Run("existing repository is not recreated", func(t *testing.T) {
		posted := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/repos/codefly-dev/authkit-go":
				fmt.Fprint(w, `{"name":"authkit-go"}`)
			case r.Method == http.MethodPost:
				posted = true
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"name":"authkit-go"}`)
			default:
				w.WriteHeader(http.StatusInternalServerError)
			}
		}))
		defer srv.Close()
		withTestClient(t, srv.URL)

		s := NewGitHubStore("codefly-dev")
		require.NoError(t, s.createRepositoryIfMissing(context.Background(), LanguageGo, "authkit"))
		require.False(t, posted, "a repository that already exists must not be created")
	})

	t.Run("missing repository is created public under the org", func(t *testing.T) {
		var body map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/repos/codefly-dev/authkit-go":
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"Not Found"}`)
			case r.Method == http.MethodPost && r.URL.Path == "/orgs/codefly-dev/repos":
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"name":"authkit-go"}`)
			default:
				w.WriteHeader(http.StatusInternalServerError)
			}
		}))
		defer srv.Close()
		withTestClient(t, srv.URL)

		s := NewGitHubStore("codefly-dev")
		require.NoError(t, s.createRepositoryIfMissing(context.Background(), LanguageGo, "authkit"))
		require.Equal(t, "authkit-go", body["name"])
		require.Equal(t, false, body["private"])
	})
}
