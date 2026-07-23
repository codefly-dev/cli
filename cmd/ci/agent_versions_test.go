package ci

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func TestProbeGitHubAssetReportsMissingArtifact(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	restore := githubAssetURL
	githubAssetURL = func(*resources.Agent) string { return server.URL + "/service-redis_0.0.74_linux_amd64.tar.gz" }
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
	githubAssetURL = func(*resources.Agent) string { return server.URL + "/asset.tar.gz" }
	defer func() { githubAssetURL = restore }()

	probe := probeGitHubAsset(context.Background(), testAgent())
	if !probe.downloadable {
		t.Fatalf("200 asset reported as not downloadable: %q", probe.detail)
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
	githubAssetURL = func(*resources.Agent) string { return github.URL + "/asset.tar.gz" }
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
