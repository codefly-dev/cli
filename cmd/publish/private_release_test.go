package publish

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/require"
)

func TestReleaseReachabilityUsesAuthenticatedLoaderAsset(t *testing.T) {
	reg, err := resources.AgentKindRegistrationFor(resources.AgentKind("codefly:service"))
	require.NoError(t, err)
	target := platform{os: runtime.GOOS, arch: runtime.GOARCH}
	assetName := reg.GitHubAsset("render", "0.1.0", target.os, target.arch)
	assetURL := loaderDownloadURL(&reg, "example.org", "render", "0.1.0", target)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/repos/example-org/service-render/releases/tags/v0.1.0":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"tag_name":"v0.1.0","assets":[{"id":1,"name":%q,"size":7,"browser_download_url":%q}]}`, assetName, assetURL)
		case "/repos/example-org/service-render/releases/assets/1":
			w.Header().Set("Content-Type", "application/octet-stream")
			fmt.Fprint(w, "archive")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	endpoint := api.URL + "/"
	client, err := github.NewClient(github.WithURLs(&endpoint, &endpoint), github.WithAuthToken("test-token"))
	require.NoError(t, err)
	require.NoError(t, verifyReleaseAssets(t.Context(), client, &reg, "example.org", "render", "0.1.0", []loaderAsset{{platform: target}}))

	anonymous, err := github.NewClient(github.WithURLs(&endpoint, &endpoint))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	require.Error(t, verifyReleaseAssets(ctx, anonymous, &reg, "example.org", "render", "0.1.0", []loaderAsset{{platform: target}}))
}
