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
	s.ensureRepo = func(context.Context, Language, string) error { return nil }
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

func TestGitHubStorePublishIgnoresAmbientSigningAndHookConfig(t *testing.T) {
	ctx := context.Background()
	// A global git config that forces signing with a bogus program, or points
	// core.hooksPath at a hook that fails outside the user's own projects,
	// would break an unguarded commit or tag; Publish must neutralize both.
	hooks := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\nexit 1\n"), 0o755))
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(globalConfig,
		[]byte("[commit]\n\tgpgsign = true\n[tag]\n\tgpgsign = true\n[gpg]\n\tprogram = /bin/false\n[core]\n\thooksPath = "+hooks+"\n"), 0o644))
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	remote := bareRepo(t)
	store := storeTo(remote)
	modulePath := goModulePath(remote)

	_, err := store.Publish(ctx, goModule(t, modulePath, "package authkit\n"),
		Coordinates{Language: LanguageGo, Name: "authkit", Version: "1.0.0"})
	require.NoError(t, err)
}

func TestGitHubStoreRejectsNonSemverAndUnsupportedLanguages(t *testing.T) {
	ctx := context.Background()
	store := storeTo(bareRepo(t))

	_, err := store.Publish(ctx, t.TempDir(), Coordinates{Language: LanguageGo, Name: "authkit", Version: "latest"})
	require.Error(t, err)

	// Build metadata is valid semver but not a valid Go module version: the go
	// command discards it, so the tag could never be fetched once pushed.
	_, err = store.Publish(ctx, t.TempDir(), Coordinates{Language: LanguageGo, Name: "authkit", Version: "1.0.0+build.1"})
	require.ErrorContains(t, err, "build metadata")

	_, err = store.Publish(ctx, t.TempDir(), Coordinates{Language: LanguagePython, Name: "authkit", Version: "1.0.0"})
	require.ErrorContains(t, err, "not implemented")
}

func TestGitHubStoreRejectsUnsafeCoordinates(t *testing.T) {
	ctx := context.Background()
	// The default remoteFor is kept: validation must fire before any remote is
	// contacted, so a traversal name never reaches a git command at all.
	store := NewGitHubStore("codefly-dev")

	// git's HTTP client normalizes "owner/../../evil/repo" to a different
	// repository, so a crafted name would redirect a credentialed publish.
	_, err := store.Publish(ctx, t.TempDir(), Coordinates{Language: LanguageGo, Name: "../../evil/lib", Version: "1.0.0"})
	require.ErrorContains(t, err, "invalid library name")
	_, err = store.Resolve(ctx, LanguageGo, "../../evil/lib", "^1.0.0")
	require.ErrorContains(t, err, "invalid library name")
	_, err = store.List(ctx, LanguageGo, "--upload-pack=evil")
	require.ErrorContains(t, err, "invalid library name")
	_, err = store.List(ctx, LanguageGo, "a..b")
	require.ErrorContains(t, err, "invalid library name")

	store.Owner = "codefly-dev/../evil"
	_, err = store.List(ctx, LanguageGo, "authkit")
	require.ErrorContains(t, err, "invalid owner")
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

func TestTreeDigestAgreesAcrossLineEndingConfig(t *testing.T) {
	ctx := context.Background()
	store := NewGitHubStore("codefly-dev")

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}

	// Publish side: core.autocrlf=input stores an LF blob while the working
	// tree keeps CRLF bytes. A digest over working-tree bytes would differ from
	// a plain clone's LF checkout; a digest over blob IDs cannot.
	pub := t.TempDir()
	run(pub, "init", "--quiet")
	run(pub, "config", "core.autocrlf", "input")
	run(pub, "config", "user.email", "t@t")
	run(pub, "config", "user.name", "t")
	run(pub, "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(pub, "lib.go"), []byte("package x\r\n"), 0o644))
	run(pub, "add", "-A")
	run(pub, "commit", "--quiet", "-m", "x")

	clone := filepath.Join(t.TempDir(), "clone")
	run(pub, "clone", "--quiet", "-c", "core.autocrlf=false", pub, clone)

	dPub, err := store.treeDigest(ctx, pub)
	require.NoError(t, err)
	dClone, err := store.treeDigest(ctx, clone)
	require.NoError(t, err)
	require.Equal(t, dPub, dClone)
}

func TestOutputSurfacesGitStderr(t *testing.T) {
	ctx := context.Background()
	store := storeTo(filepath.Join(t.TempDir(), "missing.git"))

	// A failing ls-remote must carry git's own diagnosis, not an opaque
	// "exit status 128" that hides an auth failure from a typo'd remote.
	_, err := store.List(ctx, LanguageGo, "authkit")
	require.Error(t, err)
	require.Contains(t, err.Error(), "repository")
}

func TestParseTagListingPeeledWinsAndLightweightFallsBack(t *testing.T) {
	annotatedObject := strings.Repeat("a", 40)
	annotatedCommit := strings.Repeat("b", 40)
	lightweightCommit := strings.Repeat("c", 40)
	nonCanonicalCommit := strings.Repeat("f", 40)
	out := annotatedObject + "\trefs/tags/v1.0.0\n" +
		annotatedCommit + "\trefs/tags/v1.0.0^{}\n" +
		lightweightCommit + "\trefs/tags/v2.0.0\n" +
		strings.Repeat("d", 40) + "\trefs/heads/main\n" +
		strings.Repeat("e", 40) + "\trefs/tags/not-semver\n" +
		nonCanonicalCommit + "\trefs/tags/v3.0\n"

	tagged := parseTagListing(out)
	require.Len(t, tagged, 3)
	// A non-canonical tag keeps its verbatim name: reconstructing "v3.0.0" from
	// the parsed version would name a ref that does not exist.
	require.Equal(t, "3.0.0", tagged[0].version.String())
	require.Equal(t, "v3.0", tagged[0].tag)
	require.Equal(t, nonCanonicalCommit, tagged[0].ref)
	// A lightweight tag's hash is already the commit.
	require.Equal(t, "2.0.0", tagged[1].version.String())
	require.Equal(t, "v2.0.0", tagged[1].tag)
	require.Equal(t, lightweightCommit, tagged[1].ref)
	// The peeled commit wins over the annotated tag object.
	require.Equal(t, "1.0.0", tagged[2].version.String())
	require.Equal(t, "v1.0.0", tagged[2].tag)
	require.Equal(t, annotatedCommit, tagged[2].ref)
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

	// Owner is read live, so reassigning it after construction retargets the
	// store instead of silently publishing to the original owner.
	store.Owner = "acme"
	require.Equal(t, "https://github.com/acme/authkit-go.git", store.remoteFor(LanguageGo, "authkit"))
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

func TestTreeDigestCoversTrackedContentOnlyModeSensitive(t *testing.T) {
	ctx := context.Background()
	store := NewGitHubStore("codefly-dev")

	initRepo := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		for _, args := range [][]string{
			{"init", "--quiet"},
			{"config", "user.email", "t@t"},
			{"config", "user.name", "t"},
			{"config", "commit.gpgsign", "false"},
		} {
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			require.NoError(t, cmd.Run(), strings.Join(args, " "))
		}
		return dir
	}
	commit := func(t *testing.T, dir string) {
		t.Helper()
		for _, args := range [][]string{{"add", "-A"}, {"commit", "--quiet", "-m", "x"}} {
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			require.NoError(t, cmd.Run(), strings.Join(args, " "))
		}
	}

	a := initRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(a, "lib.go"), []byte("package x\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(a, ".gitignore"), []byte("secret.txt\n"), 0o644))
	commit(t, a)
	da, err := store.treeDigest(ctx, a)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(da, "sha256:"))

	// Byte-identical committed content yields an identical digest.
	b := initRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(b, "lib.go"), []byte("package x\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(b, ".gitignore"), []byte("secret.txt\n"), 0o644))
	commit(t, b)
	db, err := store.treeDigest(ctx, b)
	require.NoError(t, err)
	require.Equal(t, da, db)

	// A .gitignore'd, untracked file is never published, so it must not change
	// the digest — otherwise the publish tree and a clone would disagree.
	require.NoError(t, os.WriteFile(filepath.Join(a, "secret.txt"), []byte("shhh\n"), 0o644))
	daIgnored, err := store.treeDigest(ctx, a)
	require.NoError(t, err)
	require.Equal(t, da, daIgnored)

	// Different tracked content changes the digest.
	require.NoError(t, os.WriteFile(filepath.Join(b, "lib.go"), []byte("package x // different\n"), 0o644))
	commit(t, b)
	dbChanged, err := store.treeDigest(ctx, b)
	require.NoError(t, err)
	require.NotEqual(t, db, dbChanged)

	// Flipping the executable bit is a real change git carries into the tree.
	require.NoError(t, os.Chmod(filepath.Join(a, "lib.go"), 0o755))
	commit(t, a)
	daExec, err := store.treeDigest(ctx, a)
	require.NoError(t, err)
	require.NotEqual(t, da, daExec)
}
