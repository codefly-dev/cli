package agents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
)

func redisAgent() *resources.Agent {
	return &resources.Agent{
		Kind:      resources.ServiceAgent,
		Publisher: "codefly.dev",
		Name:      "redis",
		Version:   "0.0.74",
	}
}

func findVersion(inv inventory, version string) (versionEntry, bool) {
	for _, entry := range inv.Versions {
		if entry.Version == version {
			return entry, true
		}
	}
	return versionEntry{}, false
}

func TestBuildInventoryUnionsSourcesAndFlags(t *testing.T) {
	releases := []releaseInfo{
		{version: "0.0.74", platforms: []string{ciPlatform, "darwin_arm64"}},
		{version: "0.0.73", platforms: []string{"darwin_arm64"}}, // release exists, but no CI asset
	}
	tags := []string{"0.0.74", "0.0.73", "0.0.56"} // 0.0.56 is tag-only
	local := []string{"0.0.74", "0.0.10"}          // 0.0.10 only in cache
	pinned := []string{"0.0.74"}

	inv := buildInventory(redisAgent(), releases, tags, local, pinned, nil, false)

	cases := map[string]sourceFlags{
		"0.0.74": {Tag: true, GithubRelease: true, PinnedHere: true, LocalCache: true},
		"0.0.73": {Tag: true},
		"0.0.56": {Tag: true},
		"0.0.10": {LocalCache: true},
	}
	if len(inv.Versions) != len(cases) {
		t.Fatalf("versions = %d, want %d", len(inv.Versions), len(cases))
	}
	for version, want := range cases {
		entry, ok := findVersion(inv, version)
		if !ok {
			t.Fatalf("missing version %s", version)
		}
		if entry.Sources != want {
			t.Fatalf("version %s sources = %+v, want %+v", version, entry.Sources, want)
		}
	}

	// The host-only release (0.0.73) keeps its platform for display even though
	// it isn't CI-resolvable.
	if entry, _ := findVersion(inv, "0.0.73"); len(entry.ReleasePlatforms) != 1 || entry.ReleasePlatforms[0] != "darwin_arm64" {
		t.Fatalf("0.0.73 release platforms = %v, want [darwin_arm64]", entry.ReleasePlatforms)
	}
}

func TestBuildInventoryLatestTagBeatsLatestResolvable(t *testing.T) {
	// The module-saas-starter#3 failure mode: the newest tag has no
	// downloadable artifact, so latest-resolvable lags latest-tag.
	releases := []releaseInfo{{version: "0.0.74", platforms: []string{ciPlatform}}}
	tags := []string{"0.0.90", "0.0.74"}

	inv := buildInventory(redisAgent(), releases, tags, nil, nil, nil, false)

	if inv.LatestTag != "0.0.90" {
		t.Fatalf("latest tag = %q, want 0.0.90", inv.LatestTag)
	}
	if inv.LatestResolvable != "0.0.74" {
		t.Fatalf("latest resolvable = %q, want 0.0.74", inv.LatestResolvable)
	}
}

func TestBuildInventorySortsDescending(t *testing.T) {
	tags := []string{"0.0.56", "0.0.74", "0.0.73"}
	inv := buildInventory(redisAgent(), nil, tags, nil, nil, nil, false)
	want := []string{"0.0.74", "0.0.73", "0.0.56"}
	for i, entry := range inv.Versions {
		if entry.Version != want[i] {
			t.Fatalf("versions[%d] = %s, want %s", i, entry.Version, want[i])
		}
	}
}

func TestBuildInventoryOCIMakesVersionResolvable(t *testing.T) {
	tags := []string{"0.0.74"}        // tag-only, no release asset
	ociVersions := []string{"0.0.74"} // but published to the OCI registry
	pinned := []string{"0.0.74"}

	inv := buildInventory(redisAgent(), nil, tags, nil, pinned, ociVersions, true)

	entry, ok := findVersion(inv, "0.0.74")
	if !ok || !entry.Sources.OCI {
		t.Fatalf("OCI flag not set: %+v", entry.Sources)
	}
	if inv.LatestResolvable != "0.0.74" {
		t.Fatalf("latest resolvable = %q, want 0.0.74 via OCI", inv.LatestResolvable)
	}
	if !inv.versionResolvable("0.0.74") {
		t.Fatal("pinned version reported unresolvable despite OCI availability")
	}
}

func TestBuildInventorySurfacesOCIOnlyVersion(t *testing.T) {
	// A version present only in the OCI registry — no git tag, no release,
	// not cached, not pinned — must still appear as a resolvable row.
	inv := buildInventory(redisAgent(), nil, nil, nil, nil, []string{"0.0.99"}, true)

	entry, ok := findVersion(inv, "0.0.99")
	if !ok {
		t.Fatal("OCI-only version 0.0.99 missing from inventory")
	}
	if entry.Sources.Tag || entry.Sources.GithubRelease {
		t.Fatalf("OCI-only version wrongly flagged as tagged/released: %+v", entry.Sources)
	}
	if !entry.Sources.OCI || inv.LatestResolvable != "0.0.99" {
		t.Fatalf("OCI-only version not resolvable: sources=%+v latest=%q", entry.Sources, inv.LatestResolvable)
	}
	if inv.LatestTag != "" {
		t.Fatalf("latest tag = %q, want empty (no tags)", inv.LatestTag)
	}
}

func TestVersionsBehindCountsNewerTags(t *testing.T) {
	// The vault case from #410: pinned 0.0.15, repo main up to 0.0.22.
	tags := []string{"0.0.15", "0.0.16", "0.0.20", "0.0.22"}
	inv := buildInventory(redisAgent(), nil, tags, nil, nil, nil, false)

	if got := inv.versionsBehind("0.0.15"); got != 3 {
		t.Fatalf("versionsBehind(0.0.15) = %d, want 3", got)
	}
	if got := inv.versionsBehind("0.0.22"); got != 0 {
		t.Fatalf("versionsBehind(0.0.22) = %d, want 0 (up to date)", got)
	}
}

func TestVersionsBehindIgnoresUntaggedAndUnparseable(t *testing.T) {
	// A newer version that exists only in the local cache (no tag) is not a
	// release the pin is "behind" — only tagged versions count.
	tags := []string{"0.0.15"}
	local := []string{"0.0.30"}
	inv := buildInventory(redisAgent(), nil, tags, local, nil, nil, false)

	if got := inv.versionsBehind("0.0.15"); got != 0 {
		t.Fatalf("versionsBehind ignoring untagged cache = %d, want 0", got)
	}
	if got := inv.versionsBehind("latest"); got != 0 {
		t.Fatalf("versionsBehind(latest) = %d, want 0", got)
	}
	if got := inv.versionsBehind("not-semver"); got != 0 {
		t.Fatalf("versionsBehind(not-semver) = %d, want 0", got)
	}
}

func TestSummarizeWorkspaceAgentsReportsDrift(t *testing.T) {
	restoreReleases, restoreTags, restoreOCI := fetchReleases, fetchTags, fetchOCITags
	defer func() { fetchReleases, fetchTags, fetchOCITags = restoreReleases, restoreTags, restoreOCI }()

	fetchReleases = func(_ context.Context, _ *resources.Agent) ([]releaseInfo, error) {
		return []releaseInfo{{version: "0.0.22", platforms: []string{ciPlatform}}}, nil
	}
	fetchTags = func(_ context.Context, _ *resources.Agent) ([]string, error) {
		return []string{"0.0.15", "0.0.16", "0.0.22"}, nil
	}
	fetchOCITags = func(_ context.Context, _ *resources.Agent) (bool, []string, error) {
		return false, nil, nil
	}

	pins := []agentPin{
		{module: "secrets", agent: &resources.Agent{Kind: resources.ServiceAgent, Publisher: "codefly.dev", Name: "vault", Version: "0.0.15"}},
	}

	summaries := summarizeWorkspaceAgents(context.Background(), pins)
	if len(summaries) != 1 {
		t.Fatalf("summaries = %d, want 1", len(summaries))
	}
	if summaries[0].Behind != 2 {
		t.Fatalf("vault behind = %d, want 2 (0.0.16 and 0.0.22)", summaries[0].Behind)
	}
}

func TestBehindCell(t *testing.T) {
	if got := behindCell(0); got != "-" {
		t.Fatalf("behindCell(0) = %q, want -", got)
	}
	if got := behindCell(3); got != "3" {
		t.Fatalf("behindCell(3) = %q, want 3", got)
	}
}

func TestReleaseCellReflectsPlatforms(t *testing.T) {
	cases := []struct {
		name  string
		entry versionEntry
		want  string
	}{
		{"ci asset present", versionEntry{Sources: sourceFlags{GithubRelease: true}, ReleasePlatforms: []string{"darwin_arm64", ciPlatform}}, "✓ darwin_arm64," + ciPlatform},
		{"host only, no ci asset", versionEntry{ReleasePlatforms: []string{"darwin_arm64"}}, "✗ (darwin_arm64)"},
		{"no assets at all", versionEntry{}, "✗"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := releaseCell(tc.entry); got != tc.want {
				t.Fatalf("releaseCell = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVersionResolvableHandlesLatest(t *testing.T) {
	inv := inventory{LatestResolvable: "0.0.74"}
	if !inv.versionResolvable("latest") {
		t.Fatal("latest should be resolvable when a resolvable version exists")
	}
	empty := inventory{}
	if empty.versionResolvable("latest") {
		t.Fatal("latest should be unresolvable when nothing is resolvable")
	}
}

func TestBuildInventorySkipsNonSemverTags(t *testing.T) {
	inv := buildInventory(redisAgent(), nil, []string{"main", "0.0.74", "v-broken"}, nil, nil, nil, false)
	if len(inv.Versions) != 1 || inv.Versions[0].Version != "0.0.74" {
		t.Fatalf("versions = %+v, want only 0.0.74", inv.Versions)
	}
}

func TestLocalCacheVersionsScansAgentDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEFLY_HOME", home)

	agent := redisAgent()
	dir := filepath.Join(home, "agents", "services", agent.Publisher)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"redis__0.0.74", "redis__0.0.10", "redis__notsemver", "vault__0.0.1"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	versions := localCacheVersions(context.Background(), agent)
	if len(versions) != 2 {
		t.Fatalf("versions = %v, want redis 0.0.74 and 0.0.10", versions)
	}
	joined := strings.Join(versions, ",")
	if !strings.Contains(joined, "0.0.74") || !strings.Contains(joined, "0.0.10") {
		t.Fatalf("versions = %v, want both redis versions", versions)
	}
}

func TestNewGitHubClientAddsAuthorization(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "secret")
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
	}))
	defer server.Close()

	client, err := newGitHubClient()
	if err != nil {
		t.Fatalf("newGitHubClient: %v", err)
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

func TestNewGitHubClientUnauthenticated(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("PATH", "") // no `gh` on PATH: force the tokenless path

	client, err := newGitHubClient()
	if err != nil {
		t.Fatalf("newGitHubClient: %v", err)
	}
	if client == nil {
		t.Fatal("newGitHubClient returned a nil client")
	}
}

func TestSummarizeWorkspaceAgentsCachesAndFlagsResolvability(t *testing.T) {
	restoreReleases, restoreTags, restoreOCI := fetchReleases, fetchTags, fetchOCITags
	defer func() { fetchReleases, fetchTags, fetchOCITags = restoreReleases, restoreTags, restoreOCI }()

	var releaseCalls int
	fetchReleases = func(_ context.Context, agent *resources.Agent) ([]releaseInfo, error) {
		releaseCalls++
		if agent.Name == "redis" {
			return []releaseInfo{{version: "0.0.74", platforms: []string{ciPlatform}}}, nil
		}
		return nil, nil // vault: tag-only, unpublished
	}
	fetchTags = func(_ context.Context, agent *resources.Agent) ([]string, error) {
		if agent.Name == "redis" {
			return []string{"0.0.74"}, nil
		}
		return []string{"0.0.15"}, nil
	}
	fetchOCITags = func(_ context.Context, _ *resources.Agent) (bool, []string, error) {
		return false, nil, nil
	}

	pins := []agentPin{
		{module: "cache", agent: &resources.Agent{Kind: resources.ServiceAgent, Publisher: "codefly.dev", Name: "redis", Version: "0.0.74"}},
		{module: "session", agent: &resources.Agent{Kind: resources.ServiceAgent, Publisher: "codefly.dev", Name: "redis", Version: "0.0.74"}},
		{module: "secrets", agent: &resources.Agent{Kind: resources.ServiceAgent, Publisher: "codefly.dev", Name: "vault", Version: "0.0.15"}},
	}

	summaries := summarizeWorkspaceAgents(context.Background(), pins)

	if releaseCalls != 2 {
		t.Fatalf("release lookups = %d, want 2 (one per distinct agent repo)", releaseCalls)
	}
	if len(summaries) != 2 {
		t.Fatalf("summaries = %d, want 2 distinct pins", len(summaries))
	}

	byAgent := map[string]agentSummary{}
	for _, summary := range summaries {
		byAgent[summary.Agent] = summary
	}
	redis := byAgent["codefly.dev/redis"]
	if !redis.PinnedResolvable {
		t.Fatal("redis pin should be resolvable")
	}
	if len(redis.Modules) != 2 {
		t.Fatalf("redis modules = %v, want cache and session", redis.Modules)
	}
	vault := byAgent["codefly.dev/vault"]
	if vault.PinnedResolvable {
		t.Fatal("vault pin should be unresolvable (tag-only)")
	}
	if vault.LatestResolvable != "" {
		t.Fatalf("vault latest resolvable = %q, want empty", vault.LatestResolvable)
	}
	if vault.LatestTag != "0.0.15" {
		t.Fatalf("vault latest tag = %q, want 0.0.15", vault.LatestTag)
	}
}

// TestGithubSourceMatchesManagerDownloadURL guards against drift: githubSource
// reimplements core's unexported owner/repo mapping, so pin it to the one
// exported source of truth (manager.DownloadURL) the actual download uses.
func TestGithubSourceMatchesManagerDownloadURL(t *testing.T) {
	for _, agent := range []*resources.Agent{
		{Kind: resources.ServiceAgent, Publisher: "codefly.dev", Name: "redis", Version: "0.0.74"},
		{Kind: resources.ServiceAgent, Publisher: "acme.co.uk", Name: "multi.part", Version: "1.2.3"},
	} {
		owner, repo := githubSource(agent)
		downloadURL, err := manager.DownloadURL(agent)
		if err != nil {
			t.Fatalf("resolve download URL: %v", err)
		}
		u, err := url.Parse(downloadURL)
		if err != nil {
			t.Fatalf("parse download URL: %v", err)
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) < 2 {
			t.Fatalf("unexpected download URL path %q", u.Path)
		}
		if parts[0] != owner || parts[1] != repo {
			t.Fatalf("githubSource = %s/%s, download URL uses %s/%s", owner, repo, parts[0], parts[1])
		}
	}
}

// TestManagerDownloadURLFailsForUnknownKind pins the fallible contract this CLI
// relies on: an agent whose kind has no registration must yield an error, never
// a plausible-looking but empty/wrong URL that call sites would probe blindly.
func TestManagerDownloadURLFailsForUnknownKind(t *testing.T) {
	agent := &resources.Agent{
		Kind:      resources.AgentKind("codefly:not-a-real-kind"),
		Publisher: "codefly.dev",
		Name:      "redis",
		Version:   "0.0.74",
	}
	got, err := manager.DownloadURL(agent)
	if err == nil {
		t.Fatalf("DownloadURL(unknown kind) = %q, want error", got)
	}
	if got != "" {
		t.Fatalf("DownloadURL(unknown kind) returned URL %q alongside error", got)
	}
}

func TestFetchOCITagsListsRegistryVersions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/agents/codefly.dev/redis/tags/list" {
			w.Write([]byte(`{"name":"agents/codefly.dev/redis","tags":["0.0.74","v0.0.73"]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	t.Setenv("AGENT_REGISTRY", strings.TrimPrefix(server.URL, "http://"))
	t.Setenv("AGENT_REGISTRY_SCHEME", "http")

	configured, versions, err := fetchOCITagsFromRegistry(context.Background(), redisAgent())
	if err != nil {
		t.Fatal(err)
	}
	if !configured {
		t.Fatal("registry configured but reported otherwise")
	}
	if strings.Join(versions, ",") != "0.0.74,0.0.73" {
		t.Fatalf("versions = %v, want [0.0.74 0.0.73] with the v-prefix stripped", versions)
	}
}

func TestFetchOCITagsUnconfigured(t *testing.T) {
	t.Setenv("AGENT_REGISTRY", "")
	configured, versions, err := fetchOCITagsFromRegistry(context.Background(), redisAgent())
	if err != nil || configured || versions != nil {
		t.Fatalf("unconfigured registry: configured=%v versions=%v err=%v", configured, versions, err)
	}
}
