package composition

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	corecomposition "github.com/codefly-dev/core/composition"
	"github.com/stretchr/testify/require"
)

// A bundle retained in the artifact cache is the canonical archive of its
// directory, named by the digest of exactly those bytes, consulted before it
// is written again, rewritten when the cached file no longer matches, and what
// core's extractor turns back into the same files — the representation a host
// verifies and serves.
func TestRetainCanonicalArchiveIsContentAddressedAndReadBack(t *testing.T) {
	workspace := t.TempDir()
	bundle := filepath.Join(t.TempDir(), "dist")
	require.NoError(t, os.MkdirAll(filepath.Join(bundle, "static", "js"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "mf-manifest.json"), []byte(`{"name":"wiki"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "static", "js", "remoteEntry.js"), []byte("export const page = 1;"), 0o600))

	digest, path, err := RetainCanonicalArchive(t.Context(), workspace, bundle)
	require.NoError(t, err)
	want, wantDigest, err := corecomposition.CanonicalArchive(bundle)
	require.NoError(t, err)
	require.Equal(t, wantDigest, digest, "the digest is core's canonical archive's")
	require.Equal(t, filepath.Join(ArtifactCacheDir(workspace), strings.TrimPrefix(digest, "sha256:")), path, "retained under the digest, where an acquisition looks first")
	stored, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, want, stored)
	require.Equal(t, contentDigest(stored), digest)

	// Already retained: read, not rewritten.
	before, err := os.Stat(path)
	require.NoError(t, err)
	again, _, err := RetainCanonicalArchive(t.Context(), workspace, bundle)
	require.NoError(t, err)
	require.Equal(t, digest, again)
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, before.ModTime(), after.ModTime(), "an archive already retained is not written again")

	// A cached file that no longer hashes to its name is replaced by the
	// archive, so the cache stays content-addressed.
	require.NoError(t, os.WriteFile(path, []byte("altered"), 0o600))
	_, _, err = RetainCanonicalArchive(t.Context(), workspace, bundle)
	require.NoError(t, err)
	restored, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, want, restored)

	// What a host extracts is the bundle as built.
	extracted := t.TempDir()
	require.NoError(t, corecomposition.ExtractArchive(t.Context(), stored, extracted))
	manifest, err := os.ReadFile(filepath.Join(extracted, "mf-manifest.json"))
	require.NoError(t, err)
	require.Equal(t, `{"name":"wiki"}`, string(manifest))
	entry, err := os.ReadFile(filepath.Join(extracted, "static", "js", "remoteEntry.js"))
	require.NoError(t, err)
	require.Equal(t, "export const page = 1;", string(entry))
}
