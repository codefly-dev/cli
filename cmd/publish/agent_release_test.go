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
	got := loaderDownloadURL("codefly.dev", "go", "0.0.16", host)

	agent := &resources.Agent{Kind: resources.ServiceAgent, Publisher: "codefly.dev", Name: "go", Version: "0.0.16"}
	require.Equal(t, manager.DownloadURL(agent), got,
		"host-platform upload URL must match the install resolver byte-for-byte")
}

func TestLoaderDownloadURL_PublisherDotsBecomeDashes(t *testing.T) {
	url := loaderDownloadURL("my.org.dev", "python", "1.2.3", platform{os: "linux", arch: "amd64"})
	require.Equal(t,
		"https://github.com/my-org-dev/service-python/releases/download/v1.2.3/service-python_1.2.3_linux_amd64.tar.gz",
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
	require.NoError(t, writeLoaderArchive(binary, "go", archive))

	entries := readTarGz(t, archive)
	require.Len(t, entries, 1, "archive must hold exactly one entry")
	entry, ok := entries["service-go"]
	require.True(t, ok, "entry must be named service-<name>, got %v", keys(entries))
	require.Equal(t, payload, entry.data)
	require.NotZero(t, entry.mode&0o111, "extracted binary must be executable")
}

func TestCollectLoaderAssets_AllPlatforms(t *testing.T) {
	ci := stageCIArtifacts(t, map[string]bool{"darwin/arm64": true, "linux/amd64": true}, true)
	stage := t.TempDir()

	assets, err := collectLoaderAssets(ci, "go", "0.0.16", stage)
	require.NoError(t, err)
	require.Len(t, assets, 2)

	for _, asset := range assets {
		require.FileExists(t, asset.archivePath)
		require.Equal(t,
			loaderArchiveName("go", "0.0.16", asset.platform),
			filepath.Base(asset.archivePath))
		require.FileExists(t, asset.sbomPath, "SBOM must be staged for %s", asset.platform.target())
	}
}

func TestCollectLoaderAssets_MissingPlatformFails(t *testing.T) {
	// Only darwin/arm64 built — a publish from a host that can't produce
	// linux/amd64 must be rejected, naming the missing platform.
	ci := stageCIArtifacts(t, map[string]bool{"darwin/arm64": true}, true)
	stage := t.TempDir()

	_, err := collectLoaderAssets(ci, "go", "0.0.16", stage)
	require.Error(t, err)
	require.Contains(t, err.Error(), "linux/amd64")
	require.Contains(t, err.Error(), "required platform")
}

func TestCollectLoaderAssets_MissingSBOMStillPackages(t *testing.T) {
	ci := stageCIArtifacts(t, map[string]bool{"darwin/arm64": true, "linux/amd64": true}, false)
	stage := t.TempDir()

	assets, err := collectLoaderAssets(ci, "go", "0.0.16", stage)
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

func TestModuleAgentSelectsSourceReleaseGateWithoutLoaderAssets(t *testing.T) {
	dir := t.TempDir()
	manifest := []byte("publisher: codefly.dev\nkind: codefly:module\nname: saas-starter\nversion: 0.0.20\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent.codefly.yaml"), manifest, 0o644))

	require.NoError(t, checkAgentReleasePreconditionsForManifest(filepath.Join(dir, "agent.codefly.yaml")))
	gate, err := newAgentReleaseGate(dir, dir)
	require.NoError(t, err)
	defer gate.cleanup()
	_, ok := gate.(*moduleAgentReleaser)
	require.True(t, ok, "module agents must not use service loader-asset publishing")
}

func TestUnknownAgentKindFailsClosedDuringReleaseSelection(t *testing.T) {
	dir := t.TempDir()
	manifest := []byte("publisher: codefly.dev\nkind: codefly:toolbox\nname: web\nversion: 0.0.1\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent.codefly.yaml"), manifest, 0o644))

	_, err := newAgentReleaseGate(dir, dir)
	require.ErrorContains(t, err, `release semantics for agent kind "codefly:toolbox"`)
}

// --- helpers -------------------------------------------------------

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
