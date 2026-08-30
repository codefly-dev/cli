package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Masterminds/semver"
	"github.com/codefly-dev/cli/cmd/publish"
	"github.com/codefly-dev/core/resources"
)

// --- pure helpers --------------------------------------------------------

func TestBumpFrom(t *testing.T) {
	base := mustVersion(t, "1.2.3")
	for _, tc := range []struct {
		bump string
		want string
	}{
		{"", "1.2.4"},
		{"patch", "1.2.4"},
		{"minor", "1.3.0"},
		{"major", "2.0.0"},
	} {
		got, err := bumpFrom(base, tc.bump)
		if err != nil {
			t.Fatalf("bumpFrom(%q) error: %v", tc.bump, err)
		}
		if got.String() != tc.want {
			t.Fatalf("bumpFrom(%q) = %s, want %s", tc.bump, got.String(), tc.want)
		}
	}
	if _, err := bumpFrom(base, "sideways"); err == nil {
		t.Fatal("bumpFrom with invalid type = nil error, want failure")
	}
}

func TestMissingPlatforms(t *testing.T) {
	missing := missingPlatforms([]string{"linux_amd64", "linux_arm64"}, []string{"linux_amd64"})
	if len(missing) != 1 || missing[0] != "linux_arm64" {
		t.Fatalf("missingPlatforms = %v, want [linux_arm64]", missing)
	}
	if got := missingPlatforms([]string{"linux_amd64"}, []string{"linux_amd64", "darwin_arm64"}); len(got) != 0 {
		t.Fatalf("missingPlatforms = %v, want none", got)
	}
}

func TestReleasePRBodyIncludesPin(t *testing.T) {
	with := releasePRBody(releaseOptions{pin: "v0.3.11"}, "0.1.0", "v0.1.1")
	if !strings.Contains(with, "v0.3.11") {
		t.Fatalf("body = %q, want it to mention the pin", with)
	}
	without := releasePRBody(releaseOptions{}, "0.1.0", "v0.1.1")
	if strings.Contains(without, "pinned") {
		t.Fatalf("body = %q, want no pin line when --pin is unset", without)
	}
}

// --- version from the authoritative remote tag ---------------------------

func TestNextReleaseVersionBumpsFromRemoteTagNotManifest(t *testing.T) {
	dir := t.TempDir()
	initAgentRepo(t, dir, "0.1.0")
	// The manifest lags (0.1.0) while origin already carries a newer tag.
	gitInRepo(t, dir, "tag", "v0.1.7")
	gitInRepo(t, dir, "push", "origin", "v0.1.7")

	manifest, err := publish.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	target := &releaseTarget{workDir: dir}

	next, err := target.nextReleaseVersion(context.Background(), manifest, "patch")
	if err != nil {
		t.Fatalf("nextReleaseVersion: %v", err)
	}
	if next.String() != "0.1.8" {
		t.Fatalf("next = %s, want 0.1.8 (patch above remote tag v0.1.7, not manifest 0.1.0)", next.String())
	}
}

func TestNextReleaseVersionUsesManifestWhenNoTags(t *testing.T) {
	dir := t.TempDir()
	initAgentRepo(t, dir, "2.0.0")

	manifest, err := publish.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	target := &releaseTarget{workDir: dir}

	next, err := target.nextReleaseVersion(context.Background(), manifest, "minor")
	if err != nil {
		t.Fatalf("nextReleaseVersion: %v", err)
	}
	if next.String() != "2.1.0" {
		t.Fatalf("next = %s, want 2.1.0", next.String())
	}
}

// --- asset verification (fail loudly on a tag with no artifact) ----------

func TestVerifyPublishedAssetPassesWhenAssetShipped(t *testing.T) {
	shrinkPolls(t)
	stubReleases(t, []releaseInfo{{version: "0.1.1", platforms: []string{ciPlatform, "linux_arm64"}}})

	target := &releaseTarget{agent: &resources.Agent{Publisher: "codefly.dev", Name: "go"}}
	if err := target.verifyPublishedAsset(context.Background(), "0.1.1", "linux_arm64"); err != nil {
		t.Fatalf("verifyPublishedAsset = %v, want nil when the asset shipped", err)
	}
}

func TestVerifyPublishedAssetFailsWhenTagHasNoArtifact(t *testing.T) {
	shrinkPolls(t)
	// The tag exists as a release, but ships no downloadable asset at all.
	stubReleases(t, []releaseInfo{{version: "0.1.1", platforms: nil}})

	target := &releaseTarget{agent: &resources.Agent{Publisher: "codefly.dev", Name: "go"}}
	err := target.verifyPublishedAsset(context.Background(), "0.1.1", "")
	if err == nil {
		t.Fatal("verifyPublishedAsset = nil, want a loud failure for a tag with no artifact")
	}
	if !strings.Contains(err.Error(), "no usable artifact") {
		t.Fatalf("error = %q, want it to name the missing artifact", err)
	}
}

func TestVerifyPublishedAssetFailsWhenExtraPlatformMissing(t *testing.T) {
	shrinkPolls(t)
	stubReleases(t, []releaseInfo{{version: "0.1.1", platforms: []string{ciPlatform}}})

	target := &releaseTarget{agent: &resources.Agent{Publisher: "codefly.dev", Name: "go"}}
	err := target.verifyPublishedAsset(context.Background(), "0.1.1", "linux_arm64")
	if err == nil || !strings.Contains(err.Error(), "linux_arm64") {
		t.Fatalf("error = %v, want it to name the missing linux_arm64 asset", err)
	}
}

// --- target resolution ---------------------------------------------------

func TestResolveReleaseTargetRejectsNonAgentDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "info.codefly.yaml"), "version: 1.0.0\n")
	if _, _, err := resolveReleaseTarget(context.Background(), dir); err == nil {
		t.Fatal("resolveReleaseTarget on a non-agent repo = nil error, want rejection")
	}
}

func TestResolveReleaseTargetRejectsNonServiceKind(t *testing.T) {
	dir := t.TempDir()
	initAgentRepoKind(t, dir, "0.1.0", "codefly:module")
	_, _, err := resolveReleaseTarget(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "service agents") {
		t.Fatalf("resolveReleaseTarget = %v, want a service-only rejection", err)
	}
}

// --- end-to-end orchestration over a fake GitHub + real git repo ---------

func TestRunAgentReleaseHappyPath(t *testing.T) {
	shrinkPolls(t)
	dir := t.TempDir()
	origin := initAgentRepo(t, dir, "0.1.0")

	var gateCalledWith string
	restore := runReleaseGate
	runReleaseGate = func(_ context.Context, gateDir, pin string) error {
		gateCalledWith = gateDir + "|" + pin
		return nil
	}
	t.Cleanup(func() { runReleaseGate = restore })

	stubReleases(t, []releaseInfo{{version: "0.1.1", platforms: []string{ciPlatform}}})

	const mergeSHA = "0123456789abcdef0123456789abcdef01234567"
	var tagRefSHA string
	server := fakeGitHub(t, fakeGitHubState{mergeSHA: mergeSHA, capturedRefSHA: &tagRefSHA})
	useFakeGitHub(t, server)

	err := runAgentRelease(context.Background(), releaseOptions{dir: dir, pin: "v0.3.11", bump: "patch"})
	if err != nil {
		t.Fatalf("runAgentRelease: %v", err)
	}

	if gateCalledWith != dir+"|v0.3.11" {
		t.Fatalf("gate called with %q, want the agent dir and pin BEFORE tagging", gateCalledWith)
	}
	if tagRefSHA != mergeSHA {
		t.Fatalf("tag ref created at %q, want the merge commit %q", tagRefSHA, mergeSHA)
	}
	// The release branch (with the bump) was pushed to origin...
	branches := gitInRepo(t, origin, "branch", "--list", "release/v0.1.1")
	if !strings.Contains(branches, "release/v0.1.1") {
		t.Fatalf("origin branches = %q, want release/v0.1.1 pushed", branches)
	}
	// ...and the operator's working tree is back on main.
	if head := strings.TrimSpace(gitInRepo(t, dir, "rev-parse", "--abbrev-ref", "HEAD")); head != "main" {
		t.Fatalf("HEAD = %q, want main after the release", head)
	}
}

func TestRunAgentReleaseNoWaitStopsAfterPR(t *testing.T) {
	shrinkPolls(t)
	dir := t.TempDir()
	initAgentRepo(t, dir, "0.1.0")

	restore := runReleaseGate
	runReleaseGate = func(context.Context, string, string) error { return nil }
	t.Cleanup(func() { runReleaseGate = restore })

	var refCreated bool
	server := fakeGitHub(t, fakeGitHubState{mergeSHA: "deadbeef", refCreated: &refCreated})
	useFakeGitHub(t, server)

	if err := runAgentRelease(context.Background(), releaseOptions{dir: dir, bump: "patch", noWait: true}); err != nil {
		t.Fatalf("runAgentRelease --no-wait: %v", err)
	}
	if refCreated {
		t.Fatal("--no-wait created a tag ref, want it to stop after opening the PR")
	}
}

// --- test helpers --------------------------------------------------------

func shrinkPolls(t *testing.T) {
	t.Helper()
	mi, mt, ai, at := mergePollInterval, mergePollTimeout, assetPollInterval, assetPollTimeout
	mergePollInterval, mergePollTimeout = time.Millisecond, 200*time.Millisecond
	assetPollInterval, assetPollTimeout = time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() {
		mergePollInterval, mergePollTimeout, assetPollInterval, assetPollTimeout = mi, mt, ai, at
	})
}

func stubReleases(t *testing.T, releases []releaseInfo) {
	t.Helper()
	restore := fetchReleases
	fetchReleases = func(context.Context, *resources.Agent) ([]releaseInfo, error) { return releases, nil }
	t.Cleanup(func() { fetchReleases = restore })
}

func mustVersion(t *testing.T, v string) *semver.Version {
	t.Helper()
	parsed, err := semver.NewVersion(v)
	if err != nil {
		t.Fatalf("parse %q: %v", v, err)
	}
	return parsed
}

type fakeGitHubState struct {
	mergeSHA       string
	capturedRefSHA *string
	refCreated     *bool
}

// fakeGitHub serves the four platform calls a release makes: list + create PR,
// get PR (already merged), and create the tag ref.
func fakeGitHub(t *testing.T, state fakeGitHubState) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/pulls") && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, "[]")
		case strings.HasSuffix(path, "/pulls") && r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"number":1,"state":"open","merged":false,"html_url":"https://github.com/x/y/pull/1"}`)
		case strings.Contains(path, "/pulls/") && r.Method == http.MethodGet:
			fmt.Fprintf(w, `{"number":1,"state":"closed","merged":true,"merge_commit_sha":%q,"html_url":"https://github.com/x/y/pull/1"}`, state.mergeSHA)
		case strings.HasSuffix(path, "/git/refs") && r.Method == http.MethodPost:
			var body struct{ SHA string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			if state.capturedRefSHA != nil {
				*state.capturedRefSHA = body.SHA
			}
			if state.refCreated != nil {
				*state.refCreated = true
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"ref":"refs/tags/v0.1.1"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func useFakeGitHub(t *testing.T, server *httptest.Server) {
	t.Helper()
	t.Cleanup(server.Close)
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_API_URL", server.URL+"/api/v3/")
}

func initAgentRepo(t *testing.T, dir, version string) (origin string) {
	return initAgentRepoKind(t, dir, version, serviceAgentKind)
}

func initAgentRepoKind(t *testing.T, dir, version, kind string) (origin string) {
	t.Helper()
	originDir := t.TempDir()
	gitInRepo(t, originDir, "init", "--bare", "-b", "main")
	gitInRepo(t, dir, "init", "-b", "main")
	gitInRepo(t, dir, "remote", "add", "origin", originDir)
	gitInRepo(t, dir, "config", "commit.gpgsign", "false")
	gitInRepo(t, dir, "config", "tag.gpgsign", "false")
	gitInRepo(t, dir, "config", "user.email", "test@example.com")
	gitInRepo(t, dir, "config", "user.name", "Test")

	manifest := fmt.Sprintf("publisher: codefly.dev\nkind: %s\nname: go\nversion: %s\n", kind, version)
	writeFile(t, filepath.Join(dir, "agent.codefly.yaml"), manifest)
	gitInRepo(t, dir, "add", "agent.codefly.yaml")
	gitInRepo(t, dir, "commit", "-m", "seed")
	gitInRepo(t, dir, "push", "-u", "origin", "main")
	return originDir
}

func gitInRepo(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}
