package publish

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// TestLoaderArchiveName_MatchesInstallResolver pins the contract that
// makes uploads installable: the asset name we upload for the host
// platform must be exactly what manager.DownloadURL (the resolver
// `codefly agent install` uses) requests. If either side drifts, this
// fails.
func TestLoaderArchiveName_MatchesInstallResolver(t *testing.T) {
	host := platform{os: runtime.GOOS, arch: runtime.GOARCH}
	for _, kind := range []resources.AgentKind{resources.ServiceAgent, resources.ToolboxAgent} {
		reg := registrationFor(t, kind)
		got := loaderDownloadURL(reg, "codefly.dev", "go", "0.0.16", host)

		agent := &resources.Agent{Kind: kind, Publisher: "codefly.dev", Name: "go", Version: "0.0.16"}
		resolver, err := manager.DownloadURL(agent)
		require.NoError(t, err)
		require.Equal(t, resolver, got,
			"host-platform upload URL must match the install resolver byte-for-byte for %s", kind)
	}
}

func TestLoaderDownloadURL_PublisherDotsBecomeDashes(t *testing.T) {
	reg := registrationFor(t, resources.ServiceAgent)
	url := loaderDownloadURL(reg, "my.org.dev", "python", "1.2.3", platform{os: "linux", arch: "amd64"})
	require.Equal(t,
		"https://github.com/my-org-dev/service-python/releases/download/v1.2.3/service-python_1.2.3_linux_amd64.tar.gz",
		url)
}

func TestLoaderDownloadURL_ToolboxUsesToolboxPrefix(t *testing.T) {
	reg := registrationFor(t, resources.ToolboxAgent)
	url := loaderDownloadURL(reg, "codefly.dev", "web", "0.0.14", platform{os: "linux", arch: "amd64"})
	require.Equal(t,
		"https://github.com/codefly-dev/toolbox-web/releases/download/v0.0.14/toolbox-web_0.0.14_linux_amd64.tar.gz",
		url)
}

// TestWriteLoaderArchive_RoundTrip ensures the archive we upload contains
// exactly the single entry core's downloader extracts (service-<name>),
// with the binary's bytes and an executable mode.
func TestWriteLoaderArchive_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "go__0.0.16")
	payload := []byte("\x7fELF fake agent binary")
	require.NoError(t, os.WriteFile(binary, payload, 0o755))

	archive := filepath.Join(dir, "out.tar.gz")
	require.NoError(t, writeLoaderArchive(binary, "service-go", archive))

	entries := readTarGz(t, archive)
	require.Len(t, entries, 1, "archive must hold exactly one entry")
	entry, ok := entries["service-go"]
	require.True(t, ok, "entry must be named service-<name>, got %v", keys(entries))
	require.Equal(t, payload, entry.data)
	require.NotZero(t, entry.mode&0o111, "extracted binary must be executable")
}

func TestCollectLoaderAssets_AllPlatforms(t *testing.T) {
	reg := serviceRegistration(t)
	ci := stageCIArtifacts(t, map[string]bool{"darwin/arm64": true, "linux/amd64": true}, true)
	stage := t.TempDir()

	assets, err := collectLoaderAssets(reg, ci, "go", "0.0.16", stage)
	require.NoError(t, err)
	require.Len(t, assets, 2)

	for _, asset := range assets {
		require.FileExists(t, asset.archivePath)
		require.Equal(t,
			loaderArchiveName(reg, "go", "0.0.16", asset.platform),
			filepath.Base(asset.archivePath))
		require.FileExists(t, asset.sbomPath, "SBOM must be staged for %s", asset.platform.target())
	}
}

func TestCollectLoaderAssets_MissingPlatformFails(t *testing.T) {
	// Only darwin/arm64 built — a publish from a host that can't produce
	// linux/amd64 must be rejected, naming the missing platform.
	ci := stageCIArtifacts(t, map[string]bool{"darwin/arm64": true}, true)
	stage := t.TempDir()

	_, err := collectLoaderAssets(serviceRegistration(t), ci, "go", "0.0.16", stage)
	require.Error(t, err)
	require.Contains(t, err.Error(), "linux/amd64")
	require.Contains(t, err.Error(), "required platform")
}

func TestCollectLoaderAssets_MissingSBOMStillPackages(t *testing.T) {
	ci := stageCIArtifacts(t, map[string]bool{"darwin/arm64": true, "linux/amd64": true}, false)
	stage := t.TempDir()

	assets, err := collectLoaderAssets(serviceRegistration(t), ci, "go", "0.0.16", stage)
	require.NoError(t, err)
	require.Len(t, assets, 2)
	for _, asset := range assets {
		require.FileExists(t, asset.archivePath)
		require.Empty(t, asset.sbomPath, "no SBOM should be staged when CI produced none")
	}
}

func TestMissingLoaderPlatforms(t *testing.T) {
	for _, tc := range []struct {
		os, arch string
		want     []string
	}{
		{"darwin", "arm64", nil},                     // native darwin/arm64 + container linux/amd64
		{"linux", "amd64", []string{"darwin/arm64"}}, // container covers linux/amd64 only
		{"linux", "arm64", []string{"darwin/arm64"}}, // native arm64 unused; still no darwin
		{"windows", "amd64", []string{"darwin/arm64"}},
		{"darwin", "amd64", []string{"darwin/arm64"}}, // native is darwin/amd64, not arm64
	} {
		got := missingLoaderPlatforms(tc.os, tc.arch)
		require.Equal(t, tc.want, got, "host %s/%s", tc.os, tc.arch)
	}
}

func TestModuleAndProviderSelectSourceTagGateWithoutLoaderAssets(t *testing.T) {
	for _, tc := range []struct {
		kind, name string
		nativeOnly bool
	}{
		{"codefly:module", "saas-starter", true},
		{"codefly:provider", "stripe", false},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			dir := t.TempDir()
			manifest := []byte("publisher: codefly.dev\nkind: " + tc.kind + "\nname: " + tc.name + "\nversion: 0.1.0\n")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "agent.codefly.yaml"), manifest, 0o644))

			require.NoError(t, checkAgentReleasePreconditionsForManifest(filepath.Join(dir, "agent.codefly.yaml")))
			gate, err := newAgentReleaseGate(dir, dir)
			require.NoError(t, err)
			defer gate.cleanup()
			releaser, ok := gate.(*sourceTagReleaser)
			require.True(t, ok, "%s must publish a source tag, not loader assets", tc.kind)
			require.Equal(t, tc.nativeOnly, releaser.nativeOnly,
				"module builds native-only; provider builds every platform to catch linux-only breaks")
		})
	}
}

func TestLoaderAssetGateSelectsRegistrationAndConformance(t *testing.T) {
	// Loader-asset publishing requires a host that can build every loader
	// platform plus gh — the same gate a real service/toolbox publish hits.
	// Skip where that can't be exercised.
	if err := checkAgentReleasePreconditions(); err != nil {
		t.Skipf("host cannot exercise loader-asset publishing: %v", err)
	}
	for _, tc := range []struct {
		kind            string
		resource        resources.AgentKind
		skipConformance bool
	}{
		{"codefly:service", resources.ServiceAgent, false},
		{"codefly:toolbox", resources.ToolboxAgent, true},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			dir := t.TempDir()
			manifest := []byte("publisher: codefly.dev\nkind: " + tc.kind + "\nname: web\nversion: 0.0.14\n")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "agent.codefly.yaml"), manifest, 0o644))

			gate, err := newAgentReleaseGate(dir, dir)
			require.NoError(t, err)
			defer gate.cleanup()
			releaser, ok := gate.(*agentReleaser)
			require.True(t, ok, "%s must ship loader assets", tc.kind)
			require.Equal(t, tc.resource, releaser.reg.Resource)
			require.Equal(t, tc.skipConformance, releaser.skipConformance,
				"conformance is service-only; %s must skip=%v", tc.kind, tc.skipConformance)
		})
	}
}

func TestReleaseAgentCIArgs(t *testing.T) {
	base := []string{"--timestamps=false", "agent", "ci", "--dir", "/d", "--output", "/o"}
	require.Equal(t, base, releaseAgentCIArgs("/d", "/o", false, false))
	require.Equal(t, append(append([]string{}, base...), "--skip-conformance"),
		releaseAgentCIArgs("/d", "/o", false, true))
	require.Equal(t, append(append([]string{}, base...), "--native-only"),
		releaseAgentCIArgs("/d", "/o", true, false))
	require.Equal(t, append(append([]string{}, base...), "--native-only", "--skip-conformance"),
		releaseAgentCIArgs("/d", "/o", true, true))
}

func TestUnsupportedAgentKindFailsClosedWithActionableError(t *testing.T) {
	dir := t.TempDir()
	manifest := []byte("publisher: codefly.dev\nkind: codefly:job\nname: batch\nversion: 0.0.1\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent.codefly.yaml"), manifest, 0o644))

	_, err := newAgentReleaseGate(dir, dir)
	require.ErrorContains(t, err, "publish supports")
	require.ErrorContains(t, err, "codefly:service")
	require.ErrorContains(t, err, `got "codefly:job"`)
}

// --- helpers -------------------------------------------------------

func serviceRegistration(t *testing.T) *resources.AgentKindRegistration {
	t.Helper()
	return registrationFor(t, resources.ServiceAgent)
}

func registrationFor(t *testing.T, kind resources.AgentKind) *resources.AgentKindRegistration {
	t.Helper()
	reg, err := resources.AgentKindRegistrationFor(kind)
	require.NoError(t, err)
	return &reg
}

// stageCIArtifacts writes a fake `codefly agent ci` output tree (report
// plus binary/SBOM files) for the given platforms and returns its root.
func stageCIArtifacts(t *testing.T, platforms map[string]bool, withSBOM bool) string {
	t.Helper()
	root := t.TempDir()
	var lines []string
	for target := range platforms {
		dir := strings.ReplaceAll(target, "/", "-")
		binRel := filepath.Join("artifacts", dir, "go__0.0.16")
		require.NoError(t, os.MkdirAll(filepath.Join(root, "artifacts", dir), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, binRel), []byte("binary-"+target), 0o755))
		lines = append(lines, `{"kind":"agent-binary","path":"`+filepath.ToSlash(binRel)+`","target":"`+target+`"}`)
		if withSBOM {
			sbomRel := binRel + ".cdx.json"
			require.NoError(t, os.WriteFile(filepath.Join(root, sbomRel), []byte(`{"bomFormat":"CycloneDX"}`), 0o644))
			lines = append(lines, `{"kind":"cyclonedx-sbom","path":"`+filepath.ToSlash(sbomRel)+`","target":"`+target+`"}`)
		}
	}
	report := `{"artifacts":[` + strings.Join(lines, ",") + `]}`
	require.NoError(t, os.WriteFile(filepath.Join(root, "report.json"), []byte(report), 0o644))
	return root
}

type tarEntry struct {
	data []byte
	mode int64
}

func readTarGz(t *testing.T, path string) map[string]tarEntry {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	defer gz.Close()
	out := map[string]tarEntry{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		data, err := io.ReadAll(tr)
		require.NoError(t, err)
		out[hdr.Name] = tarEntry{data: data, mode: hdr.Mode}
	}
	return out
}

func keys(m map[string]tarEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
