package librarystore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func pythonDistribution(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(body), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "signkit.py"), []byte("VERSION = 1\n"), 0o644))
	return dir
}

func TestGitHubStorePublishResolvePythonLibrary(t *testing.T) {
	ctx := context.Background()
	remote := bareRepo(t)
	store := storeTo(remote)

	published, err := store.Publish(ctx, pythonDistribution(t, "[project]\nname = \"signkit\"\nversion = \"1.0.0\"\n"),
		Coordinates{Language: LanguagePython, Name: "signkit", Version: "1.0.0"})
	require.NoError(t, err)
	require.Equal(t, "signkit", published.Name)
	require.Equal(t, "1.0.0", published.Version)
	require.True(t, strings.HasPrefix(published.InstallHint, `pip install "git+`), "InstallHint %q must be a pip git install", published.InstallHint)
	require.Contains(t, published.InstallHint, "@v1.0.0")
	require.True(t, strings.HasPrefix(published.Digest, "sha256:"))

	versions, err := store.List(ctx, LanguagePython, "signkit")
	require.NoError(t, err)
	require.Equal(t, []string{"1.0.0"}, versions)

	resolved, err := store.Resolve(ctx, LanguagePython, "signkit", "^1.0.0")
	require.NoError(t, err)
	require.Equal(t, "1.0.0", resolved.Version)
	require.Equal(t, published.Ref, resolved.Ref)
	require.Equal(t, published.Digest, resolved.Digest)
	require.True(t, strings.HasPrefix(resolved.InstallHint, `pip install "git+`))
}

func TestGitHubStorePublishPythonRequiresPyprojectToml(t *testing.T) {
	ctx := context.Background()
	store := storeTo(bareRepo(t))

	_, err := store.Publish(ctx, t.TempDir(), Coordinates{Language: LanguagePython, Name: "signkit", Version: "1.0.0"})
	require.ErrorContains(t, err, "pyproject.toml")
}
