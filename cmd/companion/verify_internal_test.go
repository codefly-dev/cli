package companion

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeManifest(t *testing.T, root, name, version string, dockerfile, flake bool) {
	t.Helper()
	dir := filepath.Join(root, "companions", name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "info.codefly.yaml"),
		[]byte("version: "+version+"\n"), 0o600))
	if dockerfile {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine\n"), 0o600))
	}
	if flake {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "flake.nix"), []byte("{}\n"), 0o600))
	}
}

func TestImageCompanions_DropsNonImageCompanions(t *testing.T) {
	in := []*Companion{
		{Name: "codefly", HasDockerfile: true},
		{Name: "proto", HasFlake: true},
		{Name: "golang"}, // info.codefly.yaml only — no image to verify
	}
	got := imageCompanions(in)
	require.Len(t, got, 2)
	require.Equal(t, "codefly", got[0].Name)
	require.Equal(t, "proto", got[1].Name)
}

func TestIsManifestNotFound(t *testing.T) {
	for _, s := range []string{
		"no such manifest: codeflydev/proto:0.0.11",
		"manifest unknown",
		"MANIFEST UNKNOWN: manifest unknown", // case-insensitive
	} {
		require.True(t, isManifestNotFound(s), "%q should be classified as missing", s)
	}
	// Anything that isn't an explicit manifest-not-found must be surfaced as
	// a real error, not silently treated as a missing tag — including
	// repository/auth failures that merely contain "not found".
	for _, s := range []string{
		"error during connect: dial tcp: i/o timeout",
		"unauthorized: authentication required",
		"denied: requested access to the resource is denied",
		"repository codeflydev/proto not found",
		"",
	} {
		require.False(t, isManifestNotFound(s), "%q must not be classified as missing", s)
	}
}

func TestResolveCoreDir_ErrorsWhenNoCompanionsDir(t *testing.T) {
	_, err := resolveCoreDir(t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "companions directory not found")
}

func TestResolveCoreDir_AcceptsExplicitFlag(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "proto", "0.0.11", true, false)
	got, err := resolveCoreDir(root)
	require.NoError(t, err)
	require.Equal(t, root, got)
}

func TestSelectTargets_SingleByName(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "proto", "0.0.11", true, false)
	got, err := selectTargets(root, false, []string{"proto"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "codeflydev/proto:0.0.11", got[0].Tag())
}

func TestSelectTargets_AllRejectsNameArgument(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "proto", "0.0.11", true, false)
	_, err := selectTargets(root, true, []string{"proto"})
	require.Error(t, err, "--all combined with a name must be rejected, not silently ignore the name")
	require.Contains(t, err.Error(), "cannot combine --all")
}

func TestSelectTargets_AllIsBuildOrdered(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "node", "0.0.12", true, false)
	writeManifest(t, root, "codefly", "0.0.3", true, false)
	writeManifest(t, root, "golang", "0.0.10", false, false)

	got, err := selectTargets(root, true, nil)
	require.NoError(t, err)
	require.Len(t, got, 3)
	// codefly base must lead so images that COPY --from it build after it.
	require.Equal(t, "codefly", got[0].Name)

	// The golang Go-package companion is a target but produces no image, so
	// verification filters it out.
	require.Len(t, imageCompanions(got), 2)
}
