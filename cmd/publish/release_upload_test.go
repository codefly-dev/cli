package publish

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/require"
)

// fakeGitHub records the release-API calls createAndUploadRelease makes so the
// create vs. clobber behavior can be asserted without reaching GitHub.
type fakeGitHub struct {
	uploaded []string // asset names uploaded, in order
	deleted  []int64  // asset ids deleted (the clobber step)
	created  bool     // whether a release was created
}

// server stands up an httptest server speaking enough of the releases API and
// returns a client pointed at it. existingAssets seeds a release already
// present for the tag (nil means the tag has no release yet).
func (f *fakeGitHub) server(t *testing.T, tag string, existingAssets map[string]int64) *github.Client {
	t.Helper()
	const releaseID = 42
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/codefly-dev/service-go/releases/tags/"+tag, func(w http.ResponseWriter, _ *http.Request) {
		if existingAssets == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeRelease(w, releaseID, existingAssets)
	})
	mux.HandleFunc("/repos/codefly-dev/service-go/releases", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		f.created = true
		writeRelease(w, releaseID, nil)
	})
	mux.HandleFunc(fmt.Sprintf("/repos/codefly-dev/service-go/releases/%d/assets", releaseID), func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		f.uploaded = append(f.uploaded, r.URL.Query().Get("name"))
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{}`)
	})
	for name, id := range existingAssets {
		mux.HandleFunc(fmt.Sprintf("/repos/codefly-dev/service-go/releases/assets/%d", id), func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, http.MethodDelete, r.Method, "existing asset %q must be deleted before re-upload", name)
			f.deleted = append(f.deleted, id)
			w.WriteHeader(http.StatusNoContent)
		})
	}

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	base := ts.URL + "/"
	client, err := github.NewClient(github.WithURLs(&base, &base))
	require.NoError(t, err)
	return client
}

func writeRelease(w http.ResponseWriter, id int64, assets map[string]int64) {
	var parts []string
	for name, aid := range assets {
		parts = append(parts, fmt.Sprintf(`{"id":%d,"name":%q}`, aid, name))
	}
	fmt.Fprintf(w, `{"id":%d,"assets":[%s]}`, id, strings.Join(parts, ","))
}

func stageAssets(t *testing.T, names ...string) []loaderAsset {
	t.Helper()
	dir := t.TempDir()
	assets := make([]loaderAsset, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte("payload-"+name), 0o644))
		assets = append(assets, loaderAsset{archivePath: path})
	}
	return assets
}

func TestCreateAndUploadRelease_CreatesWhenAbsent(t *testing.T) {
	f := &fakeGitHub{}
	client := f.server(t, "v0.0.16", nil)
	assets := stageAssets(t, "service-go_0.0.16_darwin_arm64.tar.gz", "service-go_0.0.16_linux_amd64.tar.gz")

	require.NoError(t, createAndUploadRelease(context.Background(), client, "codefly-dev", "service-go", "v0.0.16", assets))

	require.True(t, f.created, "a release must be created when the tag has none")
	require.ElementsMatch(t, []string{
		"service-go_0.0.16_darwin_arm64.tar.gz",
		"service-go_0.0.16_linux_amd64.tar.gz",
	}, f.uploaded)
	require.Empty(t, f.deleted, "nothing to clobber on a fresh release")
}

func TestCreateAndUploadRelease_ClobbersExistingAsset(t *testing.T) {
	name := "service-go_0.0.16_linux_amd64.tar.gz"
	f := &fakeGitHub{}
	client := f.server(t, "v0.0.16", map[string]int64{name: 7})
	assets := stageAssets(t, name)

	require.NoError(t, createAndUploadRelease(context.Background(), client, "codefly-dev", "service-go", "v0.0.16", assets))

	require.False(t, f.created, "an existing release must be reused, not recreated")
	require.Equal(t, []int64{7}, f.deleted, "the same-named asset must be deleted before re-upload")
	require.Equal(t, []string{name}, f.uploaded)
}
