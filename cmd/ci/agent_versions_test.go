package ci

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/resources"
)

func testAgent() *resources.Agent {
	return &resources.Agent{
		Kind:      resources.ServiceAgent,
		Publisher: "codefly.dev",
		Name:      "redis",
		Version:   "0.0.74",
	}
}

func TestPinBehind(t *testing.T) {
	cases := []struct {
		pinned, latest string
		want           bool
	}{
		{"0.0.15", "0.0.22", true},
		{"0.0.22", "0.0.22", false},
		{"0.0.23", "0.0.22", false},
		{"v0.0.15", "v0.0.22", true},
		{"not-semver", "0.0.22", false},
		{"0.0.15", "not-semver", false},
	}
	for _, tc := range cases {
		if got := pinBehind(tc.pinned, tc.latest); got != tc.want {
			t.Fatalf("pinBehind(%q, %q) = %v, want %v", tc.pinned, tc.latest, got, tc.want)
		}
	}
}

func TestWarnStaleAgentPins(t *testing.T) {
	restore := latestReleaseOf
	defer func() { latestReleaseOf = restore }()

	latestReleaseOf = func(_ context.Context, agent *resources.Agent) (string, error) {
		switch agent.Name {
		case "vault":
			return "0.0.22", nil // pinned 0.0.15 → behind
		case "redis":
			return "0.0.74", nil // pinned 0.0.74 → current
		default:
			return "", errors.New("unreachable")
		}
	}

	agents := []*resources.Agent{
		{Kind: resources.ServiceAgent, Publisher: "codefly.dev", Name: "vault", Version: "0.0.15"},
		{Kind: resources.ServiceAgent, Publisher: "codefly.dev", Name: "redis", Version: "0.0.74"},
		{Kind: resources.ServiceAgent, Publisher: "codefly.dev", Name: "latest-pin", Version: "latest"},
		{Kind: resources.ServiceAgent, Publisher: "codefly.dev", Name: "unreachable", Version: "0.0.1"},
	}

	cli.StartCapture()
	warnStaleAgentPins(context.Background(), agents)
	lines := cli.DrainCapture()

	var warnings []string
	for _, line := range lines {
		if strings.Contains(line.Message, "behind its repo main") {
			warnings = append(warnings, line.Message)
		}
	}
	if len(warnings) != 1 {
		t.Fatalf("drift warnings = %d (%v), want exactly 1 (vault only)", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "vault") || !strings.Contains(warnings[0], "0.0.22") {
		t.Fatalf("warning = %q, want it to name vault and latest release 0.0.22", warnings[0])
	}
}

func TestProbeGitHubAssetReportsMissingArtifact(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	restore := githubAssetURL
	githubAssetURL = func(*resources.Agent) (string, error) {
		return server.URL + "/service-redis_0.0.74_linux_amd64.tar.gz", nil
	}
	defer func() { githubAssetURL = restore }()

	probe := probeGitHubAsset(context.Background(), testAgent())
	if probe.downloadable {
		t.Fatal("404 asset reported as downloadable")
	}
	if probe.detail != "404" {
		t.Fatalf("detail = %q, want 404", probe.detail)
	}
	if !strings.Contains(probe.label, "service-redis_0.0.74_linux_amd64.tar.gz") {
		t.Fatalf("label = %q, want the asset filename", probe.label)
	}
}

func TestProbeGitHubAssetReportsPublishedArtifact(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	restore := githubAssetURL
	githubAssetURL = func(*resources.Agent) (string, error) { return server.URL + "/asset.tar.gz", nil }
	defer func() { githubAssetURL = restore }()

	probe := probeGitHubAsset(context.Background(), testAgent())
	if !probe.downloadable {
		t.Fatalf("200 asset reported as not downloadable: %q", probe.detail)
	}
}

func TestProbeGitHubAssetReportsUnresolvableKind(t *testing.T) {
	// An agent whose kind has no registration cannot yield an asset URL. The
	// real githubAssetURL (manager.DownloadURL) must surface that as a failed
	// probe with the resolution error, not silently pass an empty URL through
	// to an HTTP request.
	agent := &resources.Agent{
		Kind:      resources.AgentKind("codefly:not-a-real-kind"),
		Publisher: "codefly.dev",
		Name:      "redis",
		Version:   "0.0.74",
	}

	probe := probeGitHubAsset(context.Background(), agent)
	if probe.downloadable {
		t.Fatal("unresolvable agent kind reported as downloadable")
	}
	if !strings.Contains(probe.detail, "not-a-real-kind") {
		t.Fatalf("detail = %q, want the kind-resolution error", probe.detail)
	}
	if probe.label != "GitHub release asset" {
		t.Fatalf("label = %q, want the unadorned fallback label", probe.label)
	}
}

func TestProbeOCIManifestNotConfigured(t *testing.T) {
	t.Setenv("AGENT_REGISTRY", "")
	probe := probeOCIManifest(context.Background(), testAgent())
	if probe.downloadable {
		t.Fatal("unconfigured OCI reported as downloadable")
	}
	if !strings.Contains(probe.detail, "not configured") {
		t.Fatalf("detail = %q, want 'not configured'", probe.detail)
	}
}

func TestProbeOCIManifestAvailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && strings.Contains(r.URL.Path, "/manifests/0.0.74") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	registry := strings.TrimPrefix(server.URL, "http://")
	t.Setenv("AGENT_REGISTRY", registry)

	probe := probeOCIManifest(context.Background(), testAgent())
	if !probe.downloadable {
		t.Fatalf("available OCI manifest reported as not downloadable: %q", probe.detail)
	}
	want := registry + "/agents/codefly.dev/redis:0.0.74"
	if !strings.Contains(probe.label, want) {
		t.Fatalf("label = %q, want reference %q", probe.label, want)
	}
}

func TestProbeAgentArtifactCombinesSources(t *testing.T) {
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer github.Close()

	restore := githubAssetURL
	githubAssetURL = func(*resources.Agent) (string, error) { return github.URL + "/asset.tar.gz", nil }
	defer func() { githubAssetURL = restore }()
	t.Setenv("AGENT_REGISTRY", "")

	status := probeAgentArtifact(context.Background(), testAgent())
	if status.downloadable() {
		t.Fatal("agent with no reachable source reported as downloadable")
	}
	if len(status.sources) != 2 {
		t.Fatalf("sources = %d, want GitHub + OCI when Nix is unconfigured", len(status.sources))
	}
}

func TestFormatUnpublishedReportListsEverySource(t *testing.T) {
	statuses := []agentArtifactStatus{
		{
			agent: testAgent(),
			sources: []agentSourceProbe{
				{label: "GitHub release asset service-redis_0.0.74_linux_amd64.tar.gz", detail: "404"},
				{label: "OCI registry", detail: "not configured (set AGENT_REGISTRY)"},
			},
		},
	}
	report := formatUnpublishedReport(statuses)
	for _, want := range []string{
		"1 agent pin not downloadable in CI",
		"agent codefly.dev/redis:0.0.74 is not published",
		"GitHub release asset service-redis_0.0.74_linux_amd64.tar.gz: 404",
		"OCI registry: not configured (set AGENT_REGISTRY)",
		"-> tag + release the agent, or set AGENT_REGISTRY",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q\n%s", want, report)
		}
	}
}

func TestFormatUnpublishedReportPluralizes(t *testing.T) {
	statuses := []agentArtifactStatus{
		{agent: testAgent(), sources: []agentSourceProbe{{label: "GitHub", detail: "404"}}},
		{agent: &resources.Agent{Kind: resources.ServiceAgent, Publisher: "codefly.dev", Name: "vault", Version: "0.0.15"}, sources: []agentSourceProbe{{label: "GitHub", detail: "404"}}},
	}
	report := formatUnpublishedReport(statuses)
	if !strings.HasPrefix(report, "2 agent pins not downloadable in CI") {
		t.Fatalf("report = %q, want plural header", report)
	}
}
