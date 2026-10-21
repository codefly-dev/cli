package orchestration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	coreservices "github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

const (
	gitSourceURL = "https://git.example.com/team/lib.git"
	commitOne    = "1111111111111111111111111111111111111111"
	commitTwo    = "2222222222222222222222222222222222222222"
)

// recordingGit stands in for git: it records each call, creates the mirror on
// init, and fails every call whose arguments contain failOn.
func recordingGit(calls *[][]string, failOn string) func(context.Context, []string, ...string) error {
	return func(_ context.Context, _ []string, args ...string) error {
		*calls = append(*calls, args)
		if failOn != "" && slices.Contains(args, failOn) {
			return errors.New("exit status 128")
		}
		if args[0] == "init" {
			return os.MkdirAll(args[len(args)-1], 0o755)
		}
		return nil
	}
}

func gitSourceRecipe(downloads ...*builderv0.GitSourceDownload) *builderv0.DockerBuildRecipe {
	return &builderv0.DockerBuildRecipe{Name: "app", GitSourceDownloads: downloads}
}

// TestGitSourcePrefetchFetchesEachCommitOnceIntoOneMirror pins what a build
// receives: one bare mirror per repository at GitSourceMirrorPath, each declared
// commit fetched by id under GitSourceMirrorRef and verified, once per commit
// however many recipes declare it, all under one directory per flow.
func TestGitSourcePrefetchFetchesEachCommitOnceIntoOneMirror(t *testing.T) {
	var calls [][]string
	prefetch := newGitSourcePrefetch()
	prefetch.run = recordingGit(&calls, "")
	t.Cleanup(func() { require.NoError(t, prefetch.Close()) })

	recipe := gitSourceRecipe(
		&builderv0.GitSourceDownload{Repository: gitSourceURL, Commit: commitOne, MirrorContext: "gitsources"},
		&builderv0.GitSourceDownload{Repository: gitSourceURL, Commit: commitTwo, MirrorContext: "gitsources"},
	)
	mirrors, err := prefetch.mirrorsFor(context.Background(), recipe)
	require.NoError(t, err)
	root := mirrors["gitsources"]
	require.Equal(t, map[string]string{"gitsources": root}, mirrors)
	mirror := filepath.Join(root, coreservices.GitSourceMirrorPath(gitSourceURL))
	gitDir := "--git-dir=" + mirror
	require.Equal(t, [][]string{
		{"init", "--quiet", "--bare", mirror},
		{gitDir, "fetch", "--depth=1", "--end-of-options", gitSourceURL, "+" + commitOne + ":" + coreservices.GitSourceMirrorRef(commitOne)},
		{gitDir, "cat-file", "-e", commitOne + "^{commit}"},
		{gitDir, "fetch", "--depth=1", "--end-of-options", gitSourceURL, "+" + commitTwo + ":" + coreservices.GitSourceMirrorRef(commitTwo)},
		{gitDir, "cat-file", "-e", commitTwo + "^{commit}"},
	}, calls)

	calls = nil
	again, err := prefetch.mirrorsFor(context.Background(), recipe)
	require.NoError(t, err)
	require.Equal(t, mirrors, again)
	require.Empty(t, calls, "a commit fetched earlier in the flow is not fetched again")

	none, err := prefetch.mirrorsFor(context.Background(), gitSourceRecipe())
	require.NoError(t, err)
	require.Nil(t, none)

	require.NoError(t, prefetch.Close())
	require.NoDirExists(t, root, "the mirrors are removed with the flow")
}

// TestGitSourcePrefetchFallsBackToItsBranchesAndTags covers a host that refuses
// a commit it does not advertise: the commit is reached through the
// repository's branches and tags, then pinned under its mirror ref.
func TestGitSourcePrefetchFallsBackToItsBranchesAndTags(t *testing.T) {
	var calls [][]string
	prefetch := newGitSourcePrefetch()
	prefetch.run = recordingGit(&calls, "--depth=1")
	t.Cleanup(func() { _ = prefetch.Close() })

	root, err := prefetch.fetch(context.Background(), gitSourceURL, commitOne)
	require.NoError(t, err)
	gitDir := "--git-dir=" + filepath.Join(root, coreservices.GitSourceMirrorPath(gitSourceURL))
	require.Equal(t, [][]string{
		{gitDir, "fetch", "--end-of-options", gitSourceURL, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"},
		{gitDir, "update-ref", coreservices.GitSourceMirrorRef(commitOne), commitOne},
		{gitDir, "cat-file", "-e", commitOne + "^{commit}"},
	}, calls[2:])
}

func TestGitSourcePrefetchReportsACommitTheSourceLacks(t *testing.T) {
	var calls [][]string
	prefetch := newGitSourcePrefetch()
	prefetch.run = recordingGit(&calls, "cat-file")
	t.Cleanup(func() { _ = prefetch.Close() })
	_, err := prefetch.mirrorsFor(context.Background(), gitSourceRecipe(&builderv0.GitSourceDownload{Repository: gitSourceURL, Commit: commitOne, MirrorContext: "gitsources"}))
	require.ErrorContains(t, err, "recipe app git source 0")
	require.ErrorContains(t, err, "does not hold commit "+commitOne)
}

// TestGitSourcePrefetchEnvKeepsHowTheHostReachesRepositories pins the boundary:
// the host's git configuration (an insteadOf carrying a credential) reaches the
// fetch, no prompt can hang it, no maintenance runs in the mirror, nothing
// points git at another repository, and no protocol allow-list overrides the
// host's own rewrites.
func TestGitSourcePrefetchEnvKeepsHowTheHostReachesRepositories(t *testing.T) {
	env := gitSourcePrefetchEnv([]string{
		"HOME=/home/dev", "GIT_DIR=/elsewhere/.git", "GIT_TERMINAL_PROMPT=1",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url.https://token@git.example.com/.insteadOf",
		"GIT_CONFIG_VALUE_0=https://git.example.com/",
	})
	value, _ := envValue(env, "GIT_TERMINAL_PROMPT")
	require.Equal(t, "0", value)
	_, set := envValue(env, "GIT_DIR")
	require.False(t, set)
	_, set = envValue(env, "GIT_ALLOW_PROTOCOL")
	require.False(t, set)
	value, _ = envValue(env, "GIT_CONFIG_KEY_0")
	require.True(t, strings.HasPrefix(value, "url.https://token@"), "the host's own entry is kept first")
	require.Equal(t, map[string]string{
		"url.https://token@git.example.com/.insteadOf": "https://git.example.com/",
		"maintenance.auto": "false", "gc.auto": "0",
	}, gitConfigSettings(env))
}

func TestGitCommandReportsAMissingGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	require.ErrorIs(t, runGitCommand(context.Background(), nil, "version"), errGitMissing)
}

// TestGitCommandOutputHasNoURLCredentials guards the error the prefetch returns,
// which is logged verbatim: a rewritten remote carrying a token must not reach it.
func TestGitCommandOutputHasNoURLCredentials(t *testing.T) {
	err := runGitCommand(context.Background(), os.Environ(), "--git-dir="+t.TempDir(), "fetch", "https://x-access-token:not-a-real-token@git.invalid/team/lib.git")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "not-a-real-token")
}

// TestAGitSourceRecipeIsNotBuiltWithoutItsMirrors pins the refusal that keeps a
// declared source from silently falling back to a fetch inside the build.
func TestAGitSourceRecipeIsNotBuiltWithoutItsMirrors(t *testing.T) {
	recipe := gitSourceRecipe(&builderv0.GitSourceDownload{Repository: gitSourceURL, Commit: commitOne, MirrorContext: "gitsources"})
	require.True(t, recipeMissesGitSourceMirrors(recipe, nil))
	require.True(t, recipeMissesGitSourceMirrors(recipe, map[string]string{"gomodproxy": "/p"}))
	require.False(t, recipeMissesGitSourceMirrors(recipe, map[string]string{"gitsources": "/m"}))
	require.False(t, recipeMissesGitSourceMirrors(gitSourceRecipe(), nil))
}

// TestGitSourceMirrorsNormalizeLikeProxies pins the identity: a mirror path,
// fetched or not yet, reads as the placeholder, so the reuse decision can
// precede the fetch, and the mirror is passed as a named build context.
func TestGitSourceMirrorsNormalizeLikeProxies(t *testing.T) {
	recipe := gitSourceRecipe(&builderv0.GitSourceDownload{Repository: gitSourceURL, Commit: commitOne, MirrorContext: "gitsources"})
	none := resolvePrivateModuleBuild(func(string) (string, bool) { return "", false })
	normalized := func(contexts map[string]string) []string {
		args := append([]string{"buildx", "build"}, none.withProxies(contexts).buildxArgs(nil)...)
		return normalizedBuildxArgs(append(args, "/ws/api"), "/ws/api", "/ws/api/Dockerfile", contexts)
	}
	fetched := map[string]string{"gitsources": "/tmp/codefly-git-sources-1"}
	require.Contains(t, none.withProxies(fetched).buildxArgs(nil), "gitsources=/tmp/codefly-git-sources-1")
	require.Equal(t, normalized(fetched), normalized(placeholderProxies(recipe)))
}
