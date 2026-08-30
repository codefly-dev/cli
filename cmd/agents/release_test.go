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

	latest, err := target.latestRemoteTag(context.Background())
	if err != nil {
		t.Fatalf("latestRemoteTag: %v", err)
	}
	next, err := bumpFrom(releaseBase(manifest.Version, latest), "patch")
	if err != nil {
		t.Fatalf("bumpFrom: %v", err)
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

	latest, err := target.latestRemoteTag(context.Background())
	if err != nil {
		t.Fatalf("latestRemoteTag: %v", err)
	}
	if latest != nil {
		t.Fatalf("latestRemoteTag = %v, want nil (no tags pushed)", latest)
	}
	next, err := bumpFrom(releaseBase(manifest.Version, latest), "minor")
	if err != nil {
		t.Fatalf("bumpFrom: %v", err)
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

// TestRunAgentReleaseResumesMergedPRViaList reproduces the finding that
// PullRequests.List omits the `merged` bool (pull-request-simple schema): a
// re-run against a merged-but-untagged PR must tag it, not error "closed
// without merging". The List body deliberately carries merged_at and no merged.
func TestRunAgentReleaseResumesMergedPRViaList(t *testing.T) {
	shrinkPolls(t)
	dir := t.TempDir()
	initAgentRepo(t, dir, "0.1.0")

	var gateCalled bool
	restore := runReleaseGate
	runReleaseGate = func(context.Context, string, string) error { gateCalled = true; return nil }
	t.Cleanup(func() { runReleaseGate = restore })

	stubReleases(t, []releaseInfo{{version: "0.1.1", platforms: []string{ciPlatform}}})

	const mergeSHA = "abc123abc123abc123abc123abc123abc123abcd"
	var tagRefSHA string
	server := fakeGitHub(t, fakeGitHubState{
		capturedRefSHA: &tagRefSHA,
		listByHead: map[string]string{
			// Merged PR as the list endpoint really returns it: merged_at set,
			// merge_commit_sha set, NO merged field.
			"release/v0.1.1": fmt.Sprintf(
				`[{"number":5,"state":"closed","merged_at":"2021-05-05T00:00:00Z","merge_commit_sha":%q,"html_url":"https://github.com/x/y/pull/5"}]`, mergeSHA),
		},
	})
	useFakeGitHub(t, server)

	if err := runAgentRelease(context.Background(), releaseOptions{dir: dir, bump: "patch"}); err != nil {
		t.Fatalf("runAgentRelease resume = %v, want it to tag the already-merged PR", err)
	}
	if gateCalled {
		t.Fatal("gate ran on resume; the release was already gated and merged")
	}
	if tagRefSHA != mergeSHA {
		t.Fatalf("tagged at %q, want the merged PR's merge commit %q", tagRefSHA, mergeSHA)
	}
}

// TestRunAgentReleaseResumeVerifiesUnpublishedTag reproduces the version-runaway
// finding: after a tag was cut through this flow but its asset never published,
// a re-run must re-verify that tag, not bump to a higher version.
func TestRunAgentReleaseResumeVerifiesUnpublishedTag(t *testing.T) {
	shrinkPolls(t)
	dir := t.TempDir()
	initAgentRepo(t, dir, "0.1.0")
	gitInRepo(t, dir, "tag", "v0.1.4")
	gitInRepo(t, dir, "push", "origin", "v0.1.4")

	var gateCalled bool
	restore := runReleaseGate
	runReleaseGate = func(context.Context, string, string) error { gateCalled = true; return nil }
	t.Cleanup(func() { runReleaseGate = restore })

	// v0.1.4 was tagged but shipped no asset.
	stubReleases(t, []releaseInfo{{version: "0.1.4", platforms: nil}})

	var refCreated bool
	server := fakeGitHub(t, fakeGitHubState{
		refCreated: &refCreated,
		listByHead: map[string]string{
			"release/v0.1.4": `[{"number":4,"state":"closed","merged_at":"2021-01-01T00:00:00Z","merge_commit_sha":"deadbeef","html_url":"https://github.com/x/y/pull/4"}]`,
		},
	})
	useFakeGitHub(t, server)

	err := runAgentRelease(context.Background(), releaseOptions{dir: dir, bump: "patch"})
	if err == nil || !strings.Contains(err.Error(), "0.1.4") {
		t.Fatalf("err = %v, want a verify failure naming the unpublished tag v0.1.4", err)
	}
	if gateCalled {
		t.Fatal("gate ran; a re-run over an unpublished tag must verify it, not cut a new release")
	}
	if refCreated {
		t.Fatal("a new tag was created; the unpublished v0.1.4 must be verified, not superseded by v0.1.5")
	}
}

// TestRunAgentReleaseProceedsWhenLatestPublished is the other half: once the
// latest release IS fully published, a re-run cuts the next version (a re-tag
// cascade releases even without code changes).
func TestRunAgentReleaseProceedsWhenLatestPublished(t *testing.T) {
	shrinkPolls(t)
	dir := t.TempDir()
	initAgentRepo(t, dir, "0.1.0")
	gitInRepo(t, dir, "tag", "v0.1.4")
	gitInRepo(t, dir, "push", "origin", "v0.1.4")

	var gateCalled bool
	restore := runReleaseGate
	runReleaseGate = func(context.Context, string, string) error { gateCalled = true; return nil }
	t.Cleanup(func() { runReleaseGate = restore })

	stubReleases(t, []releaseInfo{
		{version: "0.1.4", platforms: []string{ciPlatform}},
		{version: "0.1.5", platforms: []string{ciPlatform}},
	})

	const mergeSHA = "5555555555555555555555555555555555555555"
	var tagRefSHA string
	server := fakeGitHub(t, fakeGitHubState{
		mergeSHA:       mergeSHA,
		capturedRefSHA: &tagRefSHA,
		listByHead: map[string]string{
			"release/v0.1.4": `[{"number":4,"state":"closed","merged_at":"2021-01-01T00:00:00Z","merge_commit_sha":"deadbeef","html_url":"https://github.com/x/y/pull/4"}]`,
			// release/v0.1.5 falls through to the default "[]".
		},
	})
	useFakeGitHub(t, server)

	if err := runAgentRelease(context.Background(), releaseOptions{dir: dir, bump: "patch"}); err != nil {
		t.Fatalf("runAgentRelease = %v, want it to cut v0.1.5 once v0.1.4 is published", err)
	}
	if !gateCalled {
		t.Fatal("gate did not run; a published latest release must not block the next one")
	}
	if tagRefSHA != mergeSHA {
		t.Fatalf("tagged at %q, want the new release's merge commit %q", tagRefSHA, mergeSHA)
	}
}

// TestOpenReleasePRDeletesRemoteBranchOnPRCreateFailure covers the partial-
// failure finding: if PR creation fails after the branch is pushed, the remote
// branch must be removed so a re-run isn't blocked by a non-fast-forward push.
func TestOpenReleasePRDeletesRemoteBranchOnPRCreateFailure(t *testing.T) {
	shrinkPolls(t)
	dir := t.TempDir()
	origin := initAgentRepo(t, dir, "0.1.0")

	restore := runReleaseGate
	runReleaseGate = func(context.Context, string, string) error { return nil }
	t.Cleanup(func() { runReleaseGate = restore })

	server := fakeGitHub(t, fakeGitHubState{createPRStatus: http.StatusInternalServerError})
	useFakeGitHub(t, server)

	err := runAgentRelease(context.Background(), releaseOptions{dir: dir, bump: "patch"})
	if err == nil {
		t.Fatal("runAgentRelease = nil, want the PR-create failure surfaced")
	}
	if branches := gitInRepo(t, origin, "branch", "--list", "release/v0.1.1"); strings.TrimSpace(branches) != "" {
		t.Fatalf("origin still has %q; a failed PR-create must delete the pushed branch", strings.TrimSpace(branches))
	}
	if head := strings.TrimSpace(gitInRepo(t, dir, "rev-parse", "--abbrev-ref", "HEAD")); head != "main" {
		t.Fatalf("HEAD = %q, want main after the aborted release", head)
	}
}

func TestAbortReleaseRestoresMainAndDeletesBranch(t *testing.T) {
	dir := t.TempDir()
	origin := initAgentRepo(t, dir, "0.1.0")
	gitInRepo(t, dir, "checkout", "-b", "release/v0.1.1")
	writeFile(t, filepath.Join(dir, "agent.codefly.yaml"), "publisher: codefly.dev\nkind: codefly:service\nname: go\nversion: 0.1.1\n")
	gitInRepo(t, dir, "commit", "-am", "release: v0.1.1")
	gitInRepo(t, dir, "push", "-u", "origin", "release/v0.1.1")
	// Leave an uncommitted change to prove abort force-restores.
	writeFile(t, filepath.Join(dir, "agent.codefly.yaml"), "dirty\n")

	target := &releaseTarget{workDir: dir}
	target.abortRelease("release/v0.1.1")

	if head := strings.TrimSpace(gitInRepo(t, dir, "rev-parse", "--abbrev-ref", "HEAD")); head != "main" {
		t.Fatalf("HEAD = %q, want main after abort", head)
	}
	if local := gitInRepo(t, dir, "branch", "--list", "release/v0.1.1"); strings.TrimSpace(local) != "" {
		t.Fatalf("local branch %q still exists after abort", strings.TrimSpace(local))
	}
	if remote := gitInRepo(t, origin, "branch", "--list", "release/v0.1.1"); strings.TrimSpace(remote) != "" {
		t.Fatalf("origin branch %q still exists after abort", strings.TrimSpace(remote))
	}
}

func TestAssertReleasableFailsWhenBehindOrigin(t *testing.T) {
	dir := t.TempDir()
	initAgentRepo(t, dir, "0.1.0")
	// Origin advances one commit past local main.
	writeFile(t, filepath.Join(dir, "extra.txt"), "ahead\n")
	gitInRepo(t, dir, "add", "extra.txt")
	gitInRepo(t, dir, "commit", "-m", "second")
	gitInRepo(t, dir, "push", "origin", "main")
	gitInRepo(t, dir, "reset", "--hard", "HEAD~1")

	target := &releaseTarget{workDir: dir}
	err := target.assertReleasable(context.Background())
	if err == nil || !strings.Contains(err.Error(), "behind origin/main") {
		t.Fatalf("assertReleasable = %v, want a behind-origin refusal", err)
	}
}

func TestWaitForMergeSurfacesPermanentError(t *testing.T) {
	shrinkPolls(t)
	server := fakeGitHub(t, fakeGitHubState{getPR: `{"message":"Not Found"}`, getPRStatus: http.StatusNotFound})
	useFakeGitHub(t, server)

	target := &releaseTarget{owner: "codefly-dev", repo: "service-go"}
	_, err := target.waitForMerge(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "cannot read PR") {
		t.Fatalf("waitForMerge = %v, want a permanent-error surface, not a merge timeout", err)
	}
}

func TestCreateTagRefRejectsExistingTagAtDifferentSHA(t *testing.T) {
	server := fakeGitHub(t, fakeGitHubState{
		createRefStatus: http.StatusUnprocessableEntity,
		createRefBody:   `{"message":"Reference already exists"}`,
		getRefBody:      `{"ref":"refs/tags/v0.1.1","object":{"sha":"othersha"}}`,
	})
	useFakeGitHub(t, server)

	target := &releaseTarget{owner: "codefly-dev", repo: "service-go"}
	err := target.createTagRef(context.Background(), "v0.1.1", "intendedsha")
	if err == nil || !strings.Contains(err.Error(), "already exists at") {
		t.Fatalf("createTagRef = %v, want a conflict when the tag points elsewhere", err)
	}
}

func TestCreateTagRefIdempotentWhenTagAtSameSHA(t *testing.T) {
	server := fakeGitHub(t, fakeGitHubState{
		createRefStatus: http.StatusUnprocessableEntity,
		createRefBody:   `{"message":"Reference already exists"}`,
		getRefBody:      `{"ref":"refs/tags/v0.1.1","object":{"sha":"samesha"}}`,
	})
	useFakeGitHub(t, server)

	target := &releaseTarget{owner: "codefly-dev", repo: "service-go"}
	if err := target.createTagRef(context.Background(), "v0.1.1", "samesha"); err != nil {
		t.Fatalf("createTagRef = %v, want success when the tag already points at the merge commit", err)
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
	// listByHead maps a head branch ("release/v0.1.4") to the JSON array returned
	// for GET /pulls?head=owner:branch. A branch not present returns "[]".
	listByHead map[string]string
	// createPR is the JSON for POST /pulls; defaults to an open, unmerged PR #1.
	createPR string
	// createPRStatus, when non-zero, makes POST /pulls fail with that status.
	createPRStatus int
	// getPR / getPRStatus configure GET /pulls/{n}; defaults to a merged PR.
	getPR       string
	getPRStatus int
	mergeSHA    string

	capturedRefSHA *string
	refCreated     *bool
	// createRefStatus/createRefBody configure POST /git/refs (default 201).
	createRefStatus int
	createRefBody   string
	// getRefBody is the JSON for GET /git/ref/tags/{tag} (the 422 already-exists path).
	getRefBody string
}

// fakeGitHub serves the platform calls a release makes: list + create PR, get
// PR, and create/get the tag ref. Behavior is config-driven so a test can model
// a fresh release, a merged-PR resume, an already-tagged release, or an error.
func fakeGitHub(t *testing.T, state fakeGitHubState) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/pulls") && r.Method == http.MethodGet:
			head := r.URL.Query().Get("head")
			_, branch, _ := strings.Cut(head, ":")
			body := "[]"
			if state.listByHead != nil {
				if b, ok := state.listByHead[branch]; ok {
					body = b
				}
			}
			_, _ = io.WriteString(w, body)
		case strings.HasSuffix(path, "/pulls") && r.Method == http.MethodPost:
			if state.createPRStatus != 0 {
				w.WriteHeader(state.createPRStatus)
				_, _ = io.WriteString(w, `{"message":"cannot create pull request"}`)
				return
			}
			body := state.createPR
			if body == "" {
				body = `{"number":1,"state":"open","html_url":"https://github.com/x/y/pull/1"}`
			}
			_, _ = io.WriteString(w, body)
		case strings.Contains(path, "/pulls/") && r.Method == http.MethodGet:
			if state.getPRStatus != 0 {
				w.WriteHeader(state.getPRStatus)
			}
			body := state.getPR
			if body == "" {
				body = fmt.Sprintf(`{"number":1,"state":"closed","merged":true,"merge_commit_sha":%q,"html_url":"https://github.com/x/y/pull/1"}`, state.mergeSHA)
			}
			_, _ = io.WriteString(w, body)
		case strings.HasSuffix(path, "/git/refs") && r.Method == http.MethodPost:
			var body struct{ SHA string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			if state.capturedRefSHA != nil {
				*state.capturedRefSHA = body.SHA
			}
			if state.refCreated != nil {
				*state.refCreated = true
			}
			status := state.createRefStatus
			if status == 0 {
				status = http.StatusCreated
			}
			w.WriteHeader(status)
			out := state.createRefBody
			if out == "" {
				out = `{"ref":"refs/tags/v0.1.1"}`
			}
			_, _ = io.WriteString(w, out)
		case strings.Contains(path, "/git/ref/") && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, state.getRefBody)
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
