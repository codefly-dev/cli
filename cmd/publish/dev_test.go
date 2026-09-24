package publish

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Masterminds/semver"
	blang "github.com/blang/semver"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
	"github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/require"
)

const testSHA = "abc123def4567890abc123def4567890abc12345"

func TestDevVersionIsTheCurrentVersionPlusADevPrereleaseOfTheCommit(t *testing.T) {
	version, err := DevVersion(semver.MustParse("0.1.47"), testSHA)
	require.NoError(t, err)
	require.Equal(t, "0.1.47-dev.abc123def456", version)
	require.True(t, IsDevVersion(version))
	require.True(t, IsDevVersion("v"+version))

	// Semver ranks it below the release it was built from, so it can never
	// outrank or collide with a real release.
	dev := semver.MustParse(version)
	require.True(t, dev.LessThan(semver.MustParse("0.1.47")))
	require.True(t, dev.GreaterThan(semver.MustParse("0.1.46")))

	// The strict parser the local agent resolver uses accepts it too.
	strict, err := blang.Make(version)
	require.NoError(t, err)
	require.True(t, strict.LT(blang.MustParse("0.1.47")))
}

func TestDevVersionRefusesWhatItCannotSpellAsStrictSemver(t *testing.T) {
	_, err := DevVersion(semver.MustParse("0.1.48-beta.1"), testSHA)
	require.ErrorContains(t, err, "not a plain release")
	_, err = DevVersion(semver.MustParse("0.1.47"), "abc123")
	require.ErrorContains(t, err, "40-character")
	// An all-digit prefix with a leading zero is invalid strict semver.
	_, err = DevVersion(semver.MustParse("0.1.47"), "012345678901"+strings.Repeat("a", 28))
	require.ErrorContains(t, err, "strict semver")
	version, err := DevVersion(semver.MustParse("0.1.47"), "123456789012"+strings.Repeat("a", 28))
	require.NoError(t, err)
	_, err = blang.Make(version)
	require.NoError(t, err)
}

func TestIsDevVersionOnlyMatchesDevBuilds(t *testing.T) {
	for _, v := range []string{"0.1.47", "0.1.48-beta.1", "latest", "", "0.1.47-dev", "0.1.47-dev.abc", "0.1.47-dev.ABC123DEF456"} {
		require.False(t, IsDevVersion(v), v)
	}
}

// A dev version must resolve wherever an agent version does: core's agent
// parser and validator, and the release-asset URL the loader downloads.
func TestDevVersionResolvesThroughCoreAgentResolution(t *testing.T) {
	version, err := DevVersion(semver.MustParse("0.1.47"), testSHA)
	require.NoError(t, err)
	agent, err := resources.ParseAgent(context.Background(), resources.ServiceAgent, "codefly.dev/go-grpc:"+version)
	require.NoError(t, err)
	require.Equal(t, version, agent.Version)
	_, err = agent.Proto()
	require.NoError(t, err)

	url, err := manager.DownloadURL(agent)
	require.NoError(t, err)
	reg := serviceRegistration(t)
	require.Equal(t, loaderDownloadURL(reg, "codefly.dev", "go-grpc", version, platform{os: runtime.GOOS, arch: runtime.GOARCH}), url)
	require.Contains(t, url, "/releases/download/v"+version+"/")
}

func TestDevReleaseIsAPrereleaseNeverMarkedLatest(t *testing.T) {
	dev := releaseRequest("v0.1.47-dev.abc123def456", true)
	require.True(t, dev.GetPrerelease())
	require.Equal(t, "false", dev.GetMakeLatest())

	stable := releaseRequest("v0.1.47", false)
	require.False(t, stable.GetPrerelease())
	require.Nil(t, stable.MakeLatest)

	// And that is what reaches GitHub when the dev release is created.
	var posted map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/codefly-dev/service-go/releases/tags/v0.1.47-dev.abc123def456", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	mux.HandleFunc("/repos/codefly-dev/service-go/releases", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&posted))
		fmt.Fprint(w, `{"id":42,"assets":[]}`)
	})
	mux.HandleFunc("/repos/codefly-dev/service-go/releases/42/assets", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{}`)
	})
	client := testClient(t, mux)
	require.NoError(t, publishReleaseAssets(t.Context(), client, "codefly-dev", "service-go", dev, stageAssets(t, "service-go_0.1.47-dev.abc123def456_linux_amd64.tar.gz")))
	require.Equal(t, true, posted["prerelease"])
	require.Equal(t, "false", posted["make_latest"])
}

func TestDevBuildRefusesToFillAFullRelease(t *testing.T) {
	tag := "v0.1.47-dev.abc123def456"
	f := &fakeGitHub{}
	client := f.server(t, tag, map[string]int64{}) // existing release, not a prerelease
	err := publishReleaseAssets(t.Context(), client, "codefly-dev", "service-go", releaseRequest(tag, true), stageAssets(t, "a.tar.gz"))
	require.ErrorContains(t, err, "not a prerelease")
	require.Empty(t, f.uploaded)
}

func TestWorkflowOwnedDevBuildNeedsGoReleaserPrereleases(t *testing.T) {
	dir := t.TempDir()
	require.ErrorContains(t, checkWorkflowPublishesPrereleases(dir), "no .goreleaser.yaml")
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".goreleaser.yaml"), []byte("builds: []\n"), 0o644))
	require.ErrorContains(t, checkWorkflowPublishesPrereleases(dir), "prerelease: auto")
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".goreleaser.yaml"), []byte("release:\n  prerelease: auto\n"), 0o644))
	require.NoError(t, checkWorkflowPublishesPrereleases(dir))
}

func TestPublishDevRefusesADirtyTree(t *testing.T) {
	engine, dir := devRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main // edited\n"), 0o644))
	_, err := engine.PublishDev(t.Context(), false)
	require.ErrorContains(t, err, "uncommitted changes")
	require.ErrorContains(t, err, "--allow-dirty")

	tag, err := engine.PublishDev(t.Context(), true)
	require.NoError(t, err)
	require.Regexp(t, `^v0\.1\.47-dev\.[0-9a-f]{12}$`, tag)
}

// The build runs against the dev version, and the manifest comes back
// byte-for-byte: the agent's version never moves and nothing is committed.
func TestPublishDevBuildsAtTheDevVersionAndRestoresTheManifest(t *testing.T) {
	engine, dir := devRepo(t)
	engine.DryRun = false
	manifestPath := filepath.Join(dir, "agent.codefly.yaml")
	before, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	head := strings.TrimSpace(gitOut(t, dir, "rev-parse", "HEAD"))

	var built string
	engine.BeforeCommit = func(_ context.Context, tag string) error {
		v, err := readVersion(manifestPath)
		require.NoError(t, err)
		built = v.String()
		return fmt.Errorf("stop after build")
	}
	_, err = engine.PublishDev(t.Context(), false)
	require.ErrorContains(t, err, "stop after build")
	require.Equal(t, "0.1.47-dev."+head[:12], built)

	after, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
	require.Equal(t, "0.1.47", engine.Manifest.Version.String())
	require.Equal(t, head, strings.TrimSpace(gitOut(t, dir, "rev-parse", "HEAD")))
	require.Empty(t, strings.TrimSpace(gitOut(t, dir, "status", "--porcelain")))
}

func devRepo(t *testing.T) (*Engine, string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent.codefly.yaml"), []byte("publisher: codefly.dev\nkind: codefly:service\nname: widget\nversion: 0.1.47\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644))
	gitOut(t, dir, "init", "-q", "-b", "feature")
	gitOut(t, dir, "add", ".")
	gitOut(t, dir, "-c", "user.name=Jane Doe", "-c", "user.email=user@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "init")
	manifest, err := Detect(dir)
	require.NoError(t, err)
	return &Engine{Manifest: manifest, DryRun: true, WorkDir: dir}, dir
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

func testClient(t *testing.T, mux *http.ServeMux) *github.Client {
	t.Helper()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	base := ts.URL + "/"
	client, err := github.NewClient(github.WithURLs(&base, &base))
	require.NoError(t, err)
	return client
}
