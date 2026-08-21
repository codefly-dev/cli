package librarystore

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func bareRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	require.NoError(t, exec.Command("git", "init", "--quiet", "--bare", dir).Run())
	return dir
}

func goModule(t *testing.T, modulePath, body string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module "+modulePath+"\n\ngo 1.26\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lib.go"), []byte(body), 0o644))
	return dir
}

func storeTo(remote string) *GitHubStore {
	s := NewGitHubStore("codefly-dev")
	s.remoteFor = func(Language, string) string { return remote }
	return s
}

func TestGitHubStorePublishResolveGoLibrary(t *testing.T) {
	ctx := context.Background()
	remote := bareRepo(t)
	store := storeTo(remote)

	first, err := store.Publish(ctx, goModule(t, "example.com/authkit", "package authkit\n\nconst V = 1\n"),
		Coordinates{Language: LanguageGo, Name: "authkit", Version: "1.0.0"})
	require.NoError(t, err)
	require.Equal(t, "authkit", first.Name)
	require.Equal(t, goModulePath(remote), first.ImportPath)
	require.Contains(t, first.InstallHint, "@v1.0.0")
	require.True(t, strings.HasPrefix(first.Digest, "sha256:"))
	require.Len(t, first.Ref, 40)

	// A second version publishes and lists newest-first.
	_, err = store.Publish(ctx, goModule(t, "example.com/authkit", "package authkit\n\nconst V = 2\n"),
		Coordinates{Language: LanguageGo, Name: "authkit", Version: "1.2.0"})
	require.NoError(t, err)

	versions, err := store.List(ctx, LanguageGo, "authkit")
	require.NoError(t, err)
	require.Equal(t, []string{"1.2.0", "1.0.0"}, versions)

	// A constraint resolves to the highest satisfying published version.
	resolved, err := store.Resolve(ctx, LanguageGo, "authkit", "^1.0.0")
	require.NoError(t, err)
	require.Equal(t, "1.2.0", resolved.Version)
	require.Len(t, resolved.Ref, 40)
	require.Contains(t, resolved.InstallHint, "@v1.2.0")
}

func TestGitHubStorePublishedVersionsAreImmutable(t *testing.T) {
	ctx := context.Background()
	store := storeTo(bareRepo(t))
	coords := Coordinates{Language: LanguageGo, Name: "authkit", Version: "1.0.0"}

	_, err := store.Publish(ctx, goModule(t, "example.com/authkit", "package authkit\n"), coords)
	require.NoError(t, err)

	_, err = store.Publish(ctx, goModule(t, "example.com/authkit", "package authkit // changed\n"), coords)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already published")
}

func TestGitHubStoreRejectsNonSemverAndUnsupportedLanguages(t *testing.T) {
	ctx := context.Background()
	store := storeTo(bareRepo(t))

	_, err := store.Publish(ctx, t.TempDir(), Coordinates{Language: LanguageGo, Name: "authkit", Version: "latest"})
	require.Error(t, err)

	_, err = store.Publish(ctx, t.TempDir(), Coordinates{Language: LanguagePython, Name: "authkit", Version: "1.0.0"})
	require.ErrorContains(t, err, "not implemented")
}

func TestGoModulePathAndRepositoryName(t *testing.T) {
	require.Equal(t, "authkit-go", repositoryName(LanguageGo, "authkit"))
	require.Equal(t, "github.com/codefly-dev/authkit-go", goModulePath("https://github.com/codefly-dev/authkit-go.git"))
	require.Equal(t, "github.com/codefly-dev/authkit-go", goModulePath("git@github.com:codefly-dev/authkit-go.git"))
}

func TestDefaultRemoteFor(t *testing.T) {
	store := NewGitHubStore("codefly-dev")
	require.Equal(t, "https://github.com/codefly-dev/authkit-go.git", store.remoteFor(LanguageGo, "authkit"))
}

func TestTreeDigestIsDeterministicAndContentSensitive(t *testing.T) {
	a := goModule(t, "example.com/x", "package x\n")
	b := goModule(t, "example.com/x", "package x\n")
	c := goModule(t, "example.com/x", "package x // different\n")

	da, err := treeDigest(a)
	require.NoError(t, err)
	db, err := treeDigest(b)
	require.NoError(t, err)
	dc, err := treeDigest(c)
	require.NoError(t, err)

	require.Equal(t, da, db)
	require.NotEqual(t, da, dc)
}
