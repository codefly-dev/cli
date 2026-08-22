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
	modulePath := goModulePath(remote)

	first, err := store.Publish(ctx, goModule(t, modulePath, "package authkit\n\nconst V = 1\n"),
		Coordinates{Language: LanguageGo, Name: "authkit", Version: "1.0.0"})
	require.NoError(t, err)
	require.Equal(t, "authkit", first.Name)
	require.Equal(t, modulePath, first.ImportPath)
	require.Contains(t, first.InstallHint, "@v1.0.0")
	require.True(t, strings.HasPrefix(first.Digest, "sha256:"))
	require.Len(t, first.Ref, 40)

	// A second version publishes and lists newest-first; a "v"-prefixed input
	// version is normalized to canonical form.
	second, err := store.Publish(ctx, goModule(t, modulePath, "package authkit\n\nconst V = 2\n"),
		Coordinates{Language: LanguageGo, Name: "authkit", Version: "v1.2.0"})
	require.NoError(t, err)
	require.Equal(t, "1.2.0", second.Version)

	versions, err := store.List(ctx, LanguageGo, "authkit")
	require.NoError(t, err)
	require.Equal(t, []string{"1.2.0", "1.0.0"}, versions)

	// A constraint resolves to the highest satisfying published version, with the
	// commit pinned and the digest matching what Publish recorded.
	resolved, err := store.Resolve(ctx, LanguageGo, "authkit", "^1.0.0")
	require.NoError(t, err)
	require.Equal(t, "1.2.0", resolved.Version)
	require.Equal(t, second.Ref, resolved.Ref)
	require.Equal(t, second.Digest, resolved.Digest)
	require.Contains(t, resolved.InstallHint, "@v1.2.0")
}

func TestGitHubStorePublishedVersionsAreImmutableButIdenticalContentReleases(t *testing.T) {
	ctx := context.Background()
	remote := bareRepo(t)
	store := storeTo(remote)
	modulePath := goModulePath(remote)
	coords := Coordinates{Language: LanguageGo, Name: "authkit", Version: "1.0.0"}

	_, err := store.Publish(ctx, goModule(t, modulePath, "package authkit\n"), coords)
	require.NoError(t, err)

	// Republishing an existing version is refused regardless of content.
	_, err = store.Publish(ctx, goModule(t, modulePath, "package authkit // changed\n"), coords)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already published")

	// A new version whose content is byte-identical to the previous release is a
	// valid release, not an empty-commit failure.
	identical, err := store.Publish(ctx, goModule(t, modulePath, "package authkit\n"),
		Coordinates{Language: LanguageGo, Name: "authkit", Version: "1.0.1"})
	require.NoError(t, err)
	require.Equal(t, "1.0.1", identical.Version)
}

func TestGitHubStoreResolveExcludesPrereleases(t *testing.T) {
	ctx := context.Background()
	remote := bareRepo(t)
	store := storeTo(remote)
	modulePath := goModulePath(remote)

	_, err := store.Publish(ctx, goModule(t, modulePath, "package authkit\n"),
		Coordinates{Language: LanguageGo, Name: "authkit", Version: "1.0.0"})
	require.NoError(t, err)
	_, err = store.Publish(ctx, goModule(t, modulePath, "package authkit // rc\n"),
		Coordinates{Language: LanguageGo, Name: "authkit", Version: "1.1.0-rc1"})
	require.NoError(t, err)

	versions, err := store.List(ctx, LanguageGo, "authkit")
	require.NoError(t, err)
	require.Equal(t, []string{"1.1.0-rc1", "1.0.0"}, versions)

	// A plain caret constraint never selects a prerelease.
	resolved, err := store.Resolve(ctx, LanguageGo, "authkit", "^1.0.0")
	require.NoError(t, err)
	require.Equal(t, "1.0.0", resolved.Version)
}

func TestGitHubStoreRejectsModulePathMismatchBeforeTouchingTheRemote(t *testing.T) {
	ctx := context.Background()
	// An unreachable remote proves validation fires before any git operation: a
	// clone attempt would fail with a network error, not a module-path error.
	store := storeTo("https://192.0.2.1/unreachable/authkit-go.git")

	_, err := store.Publish(ctx, goModule(t, "example.com/wrong", "package authkit\n"),
		Coordinates{Language: LanguageGo, Name: "authkit", Version: "1.0.0"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "declares module")
	require.Contains(t, err.Error(), "example.com/wrong")

	// A missing go.mod is rejected the same way.
	_, err = store.Publish(ctx, t.TempDir(),
		Coordinates{Language: LanguageGo, Name: "authkit", Version: "1.0.0"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "go.mod")
}

func TestGitHubStoreRejectsNonSemverAndUnsupportedLanguages(t *testing.T) {
	ctx := context.Background()
	store := storeTo(bareRepo(t))

	_, err := store.Publish(ctx, t.TempDir(), Coordinates{Language: LanguageGo, Name: "authkit", Version: "latest"})
	require.Error(t, err)

	_, err = store.Publish(ctx, t.TempDir(), Coordinates{Language: LanguagePython, Name: "authkit", Version: "1.0.0"})
	require.ErrorContains(t, err, "not implemented")
}

func TestReplaceTrackedTreeSkipsSourceGitAndPreservesCloneGit(t *testing.T) {
	work := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(work, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(work, "stale.go"), []byte("old"), 0o644))

	source := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(source, ".git", "refs", "heads"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(source, ".git", "config"), []byte("[core]\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(source, "lib.go"), []byte("package lib\n"), 0o644))

	require.NoError(t, replaceTrackedTree(work, source))

	// The clone's git state is untouched, the source's .git never copied, prior
	// content dropped, and new content in place.
	head, err := os.ReadFile(filepath.Join(work, ".git", "HEAD"))
	require.NoError(t, err)
	require.Equal(t, "ref: refs/heads/main\n", string(head))
	_, err = os.Stat(filepath.Join(work, ".git", "config"))
	require.True(t, os.IsNotExist(err), "source .git must not be copied into the clone")
	_, err = os.Stat(filepath.Join(work, "stale.go"))
	require.True(t, os.IsNotExist(err))
	data, err := os.ReadFile(filepath.Join(work, "lib.go"))
	require.NoError(t, err)
	require.Equal(t, "package lib\n", string(data))
}

func TestParseTagListingPeeledWinsAndLightweightFallsBack(t *testing.T) {
	annotatedObject := strings.Repeat("a", 40)
	annotatedCommit := strings.Repeat("b", 40)
	lightweightCommit := strings.Repeat("c", 40)
	out := annotatedObject + "\trefs/tags/v1.0.0\n" +
		annotatedCommit + "\trefs/tags/v1.0.0^{}\n" +
		lightweightCommit + "\trefs/tags/v2.0.0\n" +
		strings.Repeat("d", 40) + "\trefs/heads/main\n" +
		strings.Repeat("e", 40) + "\trefs/tags/not-semver\n"

	tagged := parseTagListing(out)
	require.Len(t, tagged, 2)
	// Newest first; a lightweight tag's hash is already the commit.
	require.Equal(t, "2.0.0", tagged[0].version.String())
	require.Equal(t, lightweightCommit, tagged[0].ref)
	// The peeled commit wins over the annotated tag object.
	require.Equal(t, "1.0.0", tagged[1].version.String())
	require.Equal(t, annotatedCommit, tagged[1].ref)
}

func TestIsCommitHash(t *testing.T) {
	require.True(t, isCommitHash(strings.Repeat("a", 40)))
	require.True(t, isCommitHash(strings.Repeat("0", 64)))
	require.False(t, isCommitHash(""))
	require.False(t, isCommitHash("v1.0.0"))
	require.False(t, isCommitHash(strings.Repeat("g", 40)))
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

func TestParseGoModulePath(t *testing.T) {
	path, err := parseGoModulePath([]byte("// release\nmodule github.com/x/y // comment\n\ngo 1.26\n"))
	require.NoError(t, err)
	require.Equal(t, "github.com/x/y", path)

	path, err = parseGoModulePath([]byte(`module "github.com/x/quoted"` + "\n"))
	require.NoError(t, err)
	require.Equal(t, "github.com/x/quoted", path)

	_, err = parseGoModulePath([]byte("go 1.26\n"))
	require.Error(t, err)
}

func TestTreeDigestDeterministicContentAndModeSensitiveGitExcluded(t *testing.T) {
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

	// Flipping the executable bit is a real change — git preserves it into the
	// published tree.
	require.NoError(t, os.Chmod(filepath.Join(b, "lib.go"), 0o755))
	dbExec, err := treeDigest(b)
	require.NoError(t, err)
	require.NotEqual(t, da, dbExec)

	// A non-executable mode variation git cannot preserve does not change it.
	require.NoError(t, os.Chmod(filepath.Join(a, "lib.go"), 0o640))
	daTightened, err := treeDigest(a)
	require.NoError(t, err)
	require.Equal(t, da, daTightened)

	// .git content is never part of the published tree.
	require.NoError(t, os.MkdirAll(filepath.Join(a, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(a, ".git", "config"), []byte("[core]\n"), 0o644))
	daWithGit, err := treeDigest(a)
	require.NoError(t, err)
	require.Equal(t, da, daWithGit)
}
