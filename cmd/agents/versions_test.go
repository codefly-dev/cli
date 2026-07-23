package agents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		{version: "0.0.74", hasCIAsset: true},
		{version: "0.0.73", hasCIAsset: false}, // release exists but no CI asset
	}
	tags := []string{"0.0.74", "0.0.73", "0.0.56"} // 0.0.56 is tag-only
	local := []string{"0.0.74", "0.0.10"}          // 0.0.10 only in cache
	pinned := []string{"0.0.74"}

	inv := buildInventory(redisAgent(), releases, tags, local, pinned, false, func(string) bool { return false })

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
}

func TestBuildInventoryLatestTagBeatsLatestResolvable(t *testing.T) {
	// The module-saas-starter#3 failure mode: the newest tag has no
	// downloadable artifact, so latest-resolvable lags latest-tag.
	releases := []releaseInfo{{version: "0.0.74", hasCIAsset: true}}
	tags := []string{"0.0.90", "0.0.74"}

	inv := buildInventory(redisAgent(), releases, tags, nil, nil, false, func(string) bool { return false })

	if inv.LatestTag != "0.0.90" {
		t.Fatalf("latest tag = %q, want 0.0.90", inv.LatestTag)
	}
	if inv.LatestResolvable != "0.0.74" {
		t.Fatalf("latest resolvable = %q, want 0.0.74", inv.LatestResolvable)
	}
}

func TestBuildInventorySortsDescending(t *testing.T) {
	tags := []string{"0.0.56", "0.0.74", "0.0.73"}
	inv := buildInventory(redisAgent(), nil, tags, nil, nil, false, func(string) bool { return false })
	want := []string{"0.0.74", "0.0.73", "0.0.56"}
	for i, entry := range inv.Versions {
		if entry.Version != want[i] {
			t.Fatalf("versions[%d] = %s, want %s", i, entry.Version, want[i])
		}
	}
}

func TestBuildInventoryOCIMakesVersionResolvable(t *testing.T) {
	tags := []string{"0.0.74"} // tag-only, no release asset
	oci := func(version string) bool { return version == "0.0.74" }

	inv := buildInventory(redisAgent(), nil, tags, nil, []string{"0.0.74"}, true, oci)

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
	inv := buildInventory(redisAgent(), nil, []string{"main", "0.0.74", "v-broken"}, nil, nil, false, func(string) bool { return false })
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

func TestTokenTransportAddsAuthorization(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
	}))
	defer server.Close()

	client := &http.Client{Transport: &tokenTransport{token: "secret"}}
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != "Bearer secret" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer secret")
	}
}

func TestSummarizeWorkspaceAgentsCachesAndFlagsResolvability(t *testing.T) {
	restoreReleases, restoreTags := fetchReleases, fetchTags
	defer func() { fetchReleases, fetchTags = restoreReleases, restoreTags }()

	var releaseCalls int
	fetchReleases = func(_ context.Context, agent *resources.Agent) ([]releaseInfo, error) {
		releaseCalls++
		if agent.Name == "redis" {
			return []releaseInfo{{version: "0.0.74", hasCIAsset: true}}, nil
		}
		return nil, nil // vault: tag-only, unpublished
	}
	fetchTags = func(_ context.Context, agent *resources.Agent) ([]string, error) {
		if agent.Name == "redis" {
			return []string{"0.0.74"}, nil
		}
		return []string{"0.0.15"}, nil
	}
	t.Setenv("AGENT_REGISTRY", "")

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
