package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

// stubResolvability points the release/tag/OCI/archived seams at in-memory
// fixtures so verifyRosterPlatform runs against the real roster without touching
// GitHub. releasesFor decides what each rostered agent appears to publish.
func stubResolvability(t *testing.T, releasesFor func(agent *resources.Agent) []releaseInfo) {
	t.Helper()
	restoreReleases, restoreTags, restoreOCI, restoreArchived := fetchReleases, fetchTags, fetchOCITags, repoArchived
	t.Cleanup(func() {
		fetchReleases, fetchTags, fetchOCITags, repoArchived = restoreReleases, restoreTags, restoreOCI, restoreArchived
	})
	repoArchived = func(context.Context, *resources.Agent) bool { return false }
	fetchOCITags = func(context.Context, *resources.Agent) (bool, []string, error) { return false, nil, nil }
	fetchTags = func(context.Context, *resources.Agent) ([]string, error) { return nil, nil }
	fetchReleases = func(_ context.Context, agent *resources.Agent) ([]releaseInfo, error) {
		return releasesFor(agent), nil
	}
}

func TestVerifyRosterPlatformPassesWhenEveryAgentShipsArch(t *testing.T) {
	stubResolvability(t, func(_ *resources.Agent) []releaseInfo {
		// The latest resolvable release (has the CI asset) also ships arm64.
		return []releaseInfo{{version: "9.9.9", platforms: []string{ciPlatform, "linux_arm64"}}}
	})
	if err := verifyRosterPlatform(context.Background(), "linux_arm64"); err != nil {
		t.Fatalf("verifyRosterPlatform = %v, want nil when every agent ships linux_arm64", err)
	}
}

func TestVerifyRosterPlatformFailsWhenAnAgentLacksArch(t *testing.T) {
	stubResolvability(t, func(agent *resources.Agent) []releaseInfo {
		platforms := []string{ciPlatform, "linux_arm64"}
		if agent.Name == "rust" {
			platforms = []string{ciPlatform} // amd64-only: the arm64 footgun
		}
		return []releaseInfo{{version: "9.9.9", platforms: platforms}}
	})
	err := verifyRosterPlatform(context.Background(), "linux_arm64")
	if err == nil {
		t.Fatal("verifyRosterPlatform = nil, want error when an agent has no linux_arm64 asset")
	}
	if !strings.Contains(err.Error(), "codefly.dev/rust") {
		t.Fatalf("error = %q, want it to name the arm64-less rust agent", err)
	}
}

func TestNormalizePlatformAcceptsDockerAndAssetSpelling(t *testing.T) {
	for _, in := range []string{"linux/arm64", "linux_arm64", " linux/arm64 "} {
		if got := normalizePlatform(in); got != "linux_arm64" {
			t.Fatalf("normalizePlatform(%q) = %q, want linux_arm64", in, got)
		}
	}
}
