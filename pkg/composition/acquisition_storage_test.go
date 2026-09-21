package composition

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/codefly-dev/core/composition"
	"github.com/stretchr/testify/require"
)

func TestAcquisitionCannotWriteThroughEscapedCacheAncestors(t *testing.T) {
	registry := newSelectionRegistry(t)
	artifact := registry.artifact("runtime", core.ArtifactRuntime, []byte("selected output"))
	for _, ancestor := range []string{".codefly", ".codefly/selection-artifacts"} {
		t.Run(ancestor, func(t *testing.T) {
			session := &SelectionSession{Root: t.TempDir()}
			outside := t.TempDir()
			name := strings.TrimPrefix(artifact.Digest, "sha256:")
			original := filepath.Join(outside, name)
			require.NoError(t, os.WriteFile(original, []byte("unrelated user data"), 0o600))
			link := filepath.Join(session.Root, ancestor)
			require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o700))
			require.NoError(t, os.Symlink(outside, link))
			_, err := session.acquireArtifact(t.Context(), registry.artifacts.Client(), artifact)
			require.Error(t, err)
			data, err := os.ReadFile(original)
			require.NoError(t, err)
			require.Equal(t, "unrelated user data", string(data))
			entries, err := os.ReadDir(outside)
			require.NoError(t, err)
			require.Len(t, entries, 1)
		})
	}
}

func TestAcquisitionWritesUseRetainedDirectoryHandle(t *testing.T) {
	session := &SelectionSession{Root: t.TempDir()}
	cache, err := session.openArtifactCache()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cache.Close()) })
	retained := cache.Name() + "-retained"
	require.NoError(t, os.Rename(cache.Name(), retained))
	outside := t.TempDir()
	data := []byte("selected bytes")
	digest := contentDigest(data)
	name := strings.TrimPrefix(digest, "sha256:")
	require.NoError(t, os.WriteFile(filepath.Join(outside, name), []byte("independent file"), 0o600))
	require.NoError(t, os.Symlink(outside, cache.Name()))
	_, err = retainArtifact(t.Context(), cache, digest, bytes.NewReader(data))
	require.NoError(t, err)
	stored, err := os.ReadFile(filepath.Join(retained, name))
	require.NoError(t, err)
	require.Equal(t, data, stored)
	untouched, err := os.ReadFile(filepath.Join(outside, name))
	require.NoError(t, err)
	require.Equal(t, "independent file", string(untouched))
}
