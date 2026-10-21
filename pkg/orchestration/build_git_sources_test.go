package orchestration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
// init, resolves a mirror ref to the commit it names, and fails every call whose
// arguments contain failOn.
func recordingGit(calls *[][]string, failOn string) func(context.Context, []string, ...string) (string, error) {
	return func(_ context.Context, _ []string, args ...string) (string, error) {
		*calls = append(*calls, args)
		if failOn != "" && slices.Contains(args, failOn) {
			return "", errors.New("exit status 128")
		}
		last := args[len(args)-1]
		switch {
		case args[0] == "init":
			return "", os.MkdirAll(last, 0o755)
		case slices.Contains(args, "rev-parse"):
			return strings.TrimSuffix(strings.TrimPrefix(last, "refs/commit/"), "^{commit}"), nil
		}
		return "", nil
	}
}

func gitSourceRecipe(downloads ...*builderv0.GitSourceDownload) *builderv0.DockerBuildRecipe {
	return &builderv0.DockerBuildRecipe{Name: "app", GitSourceDownloads: downloads}
}

func verifyCall(gitDir, commit string) []string {
	return []string{gitDir, "rev-parse", "--verify", "--end-of-options", coreservices.GitSourceMirrorRef(commit) + "^{commit}"}
}

// TestGitSourcePrefetchFetchesEachCommitOnceIntoOneMirror pins what a build
// receives: one bare mirror per repository at GitSourceMirrorPath, created from
// no template, each declared commit fetched by id under GitSourceMirrorRef and
// verified, once per commit however many recipes declare it, all under one
// directory per flow.
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
		{"init", "--quiet", "--bare", "--template=", "--object-format=sha1", mirror},
		{gitDir, "fetch", "--depth=1", "--end-of-options", gitSourceURL, "+" + commitOne + ":" + coreservices.GitSourceMirrorRef(commitOne)},
		verifyCall(gitDir, commitOne),
		{gitDir, "fetch", "--depth=1", "--end-of-options", gitSourceURL, "+" + commitTwo + ":" + coreservices.GitSourceMirrorRef(commitTwo)},
		verifyCall(gitDir, commitTwo),
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
		verifyCall(gitDir, commitOne),
	}, calls[2:])
}

func TestGitSourcePrefetchReportsACommitTheSourceLacks(t *testing.T) {
	var calls [][]string
	prefetch := newGitSourcePrefetch()
	prefetch.run = recordingGit(&calls, "rev-parse")
	t.Cleanup(func() { _ = prefetch.Close() })
	_, err := prefetch.mirrorsFor(context.Background(), gitSourceRecipe(&builderv0.GitSourceDownload{Repository: gitSourceURL, Commit: commitOne, MirrorContext: "gitsources"}))
	require.ErrorContains(t, err, "recipe app git source 0")
	require.ErrorContains(t, err, "does not hold commit "+commitOne)
}

// TestGitSourcePrefetchRefusesAMirrorRefNamingAnotherCommit pins that the
// mirror ref must resolve to the locked id itself.
func TestGitSourcePrefetchRefusesAMirrorRefNamingAnotherCommit(t *testing.T) {
	prefetch := newGitSourcePrefetch()
	var calls [][]string
	record := recordingGit(&calls, "")
	prefetch.run = func(ctx context.Context, env []string, args ...string) (string, error) {
		if slices.Contains(args, "rev-parse") {
			return commitTwo, nil
		}
		return record(ctx, env, args...)
	}
	t.Cleanup(func() { _ = prefetch.Close() })
	_, err := prefetch.fetch(context.Background(), gitSourceURL, commitOne)
	require.ErrorContains(t, err, "commit not available: the mirror ref of commit "+commitOne+" resolves to "+commitTwo)
}

// TestGitSourcePrefetchMatchesTheObjectFormatToTheCommit pins that a 64-digit
// id gets a SHA-256 mirror and that one repository declared with both lengths
// is refused.
func TestGitSourcePrefetchMatchesTheObjectFormatToTheCommit(t *testing.T) {
	var calls [][]string
	prefetch := newGitSourcePrefetch()
	prefetch.run = recordingGit(&calls, "")
	t.Cleanup(func() { _ = prefetch.Close() })
	long := strings.Repeat("3", 64)
	_, err := prefetch.fetch(context.Background(), gitSourceURL, long)
	require.NoError(t, err)
	require.Contains(t, calls[0], "--object-format=sha256")
	_, err = prefetch.fetch(context.Background(), gitSourceURL, commitOne)
	require.ErrorContains(t, err, "both sha256 and sha1")
}

// gitSourceRepository writes a bare repository in the given object format with
// a file at two commits, and returns its file:// URL and the first commit,
// which is not a branch tip, so fetching it by id asks for an unadvertised
// commit.
func gitSourceRepository(t *testing.T, format string) (string, string) {
	t.Helper()
	work := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = work
		command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_AUTHOR_NAME=codefly", "GIT_AUTHOR_EMAIL=codefly@example.com",
			"GIT_COMMITTER_NAME=codefly", "GIT_COMMITTER_EMAIL=codefly@example.com")
		out, err := command.CombinedOutput()
		require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main", "--object-format="+format, ".")
	require.NoError(t, os.WriteFile(filepath.Join(work, "lib.txt"), []byte("locked\n"), 0o644))
	git("add", "lib.txt")
	git("commit", "-q", "-m", "the locked commit")
	locked := git("rev-parse", "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(work, "lib.txt"), []byte("later\n"), 0o644))
	git("commit", "-q", "-am", "a later commit")
	repository := filepath.Join(t.TempDir(), "lib.git")
	git("clone", "-q", "--bare", work, repository)
	return "file://" + repository, locked
}

// hostGitConfig makes content the host's global git configuration for the test.
func hostGitConfig(t *testing.T, content string) {
	t.Helper()
	configuration := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(configuration, []byte(content), 0o644))
	t.Setenv("GIT_CONFIG_GLOBAL", configuration)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func requireGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("the prefetch runs the host git")
	}
}

// TestGitSourcePrefetchMirrorsASHA256Repository fetches a SHA-256 commit into
// a mirror of the same format.
func TestGitSourcePrefetchMirrorsASHA256Repository(t *testing.T) {
	requireGit(t)
	hostGitConfig(t, "")
	repository, locked := gitSourceRepository(t, "sha256")
	require.Len(t, locked, 64)
	prefetch := newGitSourcePrefetch()
	t.Cleanup(func() { require.NoError(t, prefetch.Close()) })
	_, err := prefetch.fetch(context.Background(), repository, locked)
	require.NoError(t, err)
}

// TestGitSourceMirrorTakesNoHostTemplate pins that a host template, which may
// carry hooks or a credential configuration, is not copied into the mirror the
// build receives.
func TestGitSourceMirrorTakesNoHostTemplate(t *testing.T) {
	requireGit(t)
	template := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(template, "config"), []byte("[credential]\n\thelper = leaked-from-template\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(template, "hooks"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(template, "hooks", "leaked-hook"), []byte("#!/bin/sh\n"), 0o755))
	hostGitConfig(t, fmt.Sprintf("[init]\n\ttemplateDir = %s\n", template))
	t.Setenv("GIT_TEMPLATE_DIR", template)
	repository, locked := gitSourceRepository(t, "sha1")

	prefetch := newGitSourcePrefetch()
	t.Cleanup(func() { require.NoError(t, prefetch.Close()) })
	root, err := prefetch.fetch(context.Background(), repository, locked)
	require.NoError(t, err)
	mirror := filepath.Join(root, coreservices.GitSourceMirrorPath(repository))
	config, err := os.ReadFile(filepath.Join(mirror, "config"))
	require.NoError(t, err)
	require.NotContains(t, string(config), "leaked-from-template")
	require.NoFileExists(t, filepath.Join(mirror, "hooks", "leaked-hook"))
}

// unauthorizedGitHost answers every request with 401, so git asks the host's
// credential helper.
func unauthorizedGitHost(t *testing.T) string {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/team/lib.git"
}

// TestGitSourcePrefetchTracesNoCredential pins that a host's git tracing,
// which prints a credential helper's command line, carries no secret into the
// error the prefetch reports and logs, nor into a trace file, whether the host
// enables it in the environment or through a trace2 target in its git
// configuration.
func TestGitSourcePrefetchTracesNoCredential(t *testing.T) {
	requireGit(t)
	const secret = "not-a-real-secret-0123"
	helper := "[credential]\n\thelper = \"!f() { echo username=ci; echo password=" + secret + "; }; f\"\n"
	for name, target := range map[string]string{
		"environment":         "",
		"trace2.normalTarget": "1",
		"trace2.eventTarget":  "file",
		"trace2.perfTarget":   "file",
	} {
		t.Run(name, func(t *testing.T) {
			traceFile := filepath.Join(t.TempDir(), "trace")
			config := helper
			switch target {
			case "":
				for _, variable := range []string{"GIT_TRACE", "GIT_TRACE_CURL", "GIT_TRACE_PACKET", "GIT_TRACE2", "GIT_CURL_VERBOSE"} {
					t.Setenv(variable, "1")
				}
			case "file":
				config += fmt.Sprintf("[trace2]\n\t%s = %s\n", strings.TrimPrefix(name, "trace2."), traceFile)
			default:
				config += fmt.Sprintf("[trace2]\n\t%s = %s\n", strings.TrimPrefix(name, "trace2."), target)
			}
			hostGitConfig(t, config)
			t.Setenv("GIT_TRACE_REDACT", "0")
			prefetch := newGitSourcePrefetch()
			t.Cleanup(func() { _ = prefetch.Close() })
			_, err := prefetch.fetch(context.Background(), unauthorizedGitHost(t), commitOne)
			require.ErrorContains(t, err, "authentication failed", "the credential helper was consulted")
			require.NotContains(t, err.Error(), secret)
			traced, _ := os.ReadFile(traceFile)
			require.NotContains(t, string(traced), secret)
		})
	}
}

// TestGitSourceFailuresAreClassifiedNotEchoed pins that git's own output never
// reaches the error the prefetch returns, which the plan phase logs: git prints
// a credential helper's command line, a missing helper's path, and what a
// helper or askpass writes to stderr, and none of it is a URL redaction can
// recognise. The failure is classified instead.
func TestGitSourceFailuresAreClassifiedNotEchoed(t *testing.T) {
	requireGit(t)
	const marker = "secret-marker-4711"
	askpass := filepath.Join(t.TempDir(), "askpass")
	require.NoError(t, os.WriteFile(askpass, []byte("#!/bin/sh\necho "+marker+" >&2\nexit 1\n"), 0o755))
	for name, setup := range map[string]func(t *testing.T){
		"a helper's command": func(t *testing.T) {
			hostGitConfig(t, "[credential]\n\thelper = \"!f() { token="+marker+"; echo quit=true; }; f\"\n")
		},
		"a missing helper": func(t *testing.T) {
			hostGitConfig(t, "[credential]\n\thelper = /nonexistent/"+marker+"\n")
		},
		"a helper's stderr": func(t *testing.T) {
			hostGitConfig(t, "[credential]\n\thelper = \"!f() { echo "+marker+" >&2; }; f\"\n")
		},
		"askpass stderr": func(t *testing.T) {
			hostGitConfig(t, "")
			t.Setenv("GIT_ASKPASS", askpass)
		},
	} {
		t.Run(name, func(t *testing.T) {
			setup(t)
			prefetch := newGitSourcePrefetch()
			t.Cleanup(func() { _ = prefetch.Close() })
			_, err := prefetch.mirrorsFor(context.Background(), gitSourceRecipe(&builderv0.GitSourceDownload{
				Repository: unauthorizedGitHost(t), Commit: commitOne, MirrorContext: "gitsources"}))
			require.Error(t, err)
			require.NotContains(t, err.Error(), marker)
			require.ErrorContains(t, err, "recipe app git source 0")
			require.ErrorContains(t, err, "git fetch exited with status 128: authentication failed")
			require.ErrorContains(t, err, "rerun")
		})
	}
}

// TestGitFailureClassification pins the fixed set of reasons a failure is
// reported as.
func TestGitFailureClassification(t *testing.T) {
	for output, reason := range map[string]string{
		"fatal: Authentication failed for 'https://h/r'":                            "authentication failed",
		"fatal: could not read Username for 'https://h': terminal prompts disabled": "authentication failed",
		"git@h: Permission denied (publickey).":                                     "authentication failed",
		"fatal: unable to access 'https://h/': Could not resolve host: h":           "host unreachable",
		"Failed to connect to h port 443: Connection refused":                       "host unreachable",
		"remote: Repository not found.":                                             "repository not found",
		"fatal: '/x' does not appear to be a git repository":                        "repository not found",
		"error: upload-pack: not our ref 1111":                                      "commit not available",
		"fatal: couldn't find remote ref 1111":                                      "commit not available",
		"fatal: something else":                                                     "git failed",
	} {
		require.Equal(t, reason, classifyGitFailure(output), output)
	}
}

// TestGitSourcePrefetchStopsAHelperWithItsContext pins that a credential helper
// still running when the context ends is stopped with git, so the fetch
// returns at the deadline instead of when the helper finishes.
func TestGitSourcePrefetchStopsAHelperWithItsContext(t *testing.T) {
	requireGit(t)
	hostGitConfig(t, "[credential]\n\thelper = \"!f() { sleep 30; }; f\"\n")
	prefetch := newGitSourcePrefetch()
	t.Cleanup(func() { _ = prefetch.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := prefetch.fetch(ctx, unauthorizedGitHost(t), commitOne)
	require.Error(t, err)
	require.Less(t, time.Since(start), gitWaitDelay, "the helper's process group is killed, not waited out")
}

// TestGitSourcePrefetchEnvKeepsHowTheHostReachesRepositories pins the boundary:
// the host's git configuration (an insteadOf carrying a credential) reaches the
// fetch, no prompt can hang it, no maintenance runs in the mirror, nothing
// points git at another repository, and no protocol allow-list overrides the
// host's own rewrites.
func TestGitSourcePrefetchEnvKeepsHowTheHostReachesRepositories(t *testing.T) {
	env := gitSourcePrefetchEnv([]string{
		"HOME=/home/dev", "GIT_DIR=/elsewhere/.git", "GIT_TERMINAL_PROMPT=1", "GIT_TEMPLATE_DIR=/t",
		"GIT_TRACE=1", "GIT_TRACE2_EVENT=/tmp/t", "GIT_CURL_VERBOSE=1",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url.https://token@git.example.com/.insteadOf",
		"GIT_CONFIG_VALUE_0=https://git.example.com/",
	})
	value, _ := envValue(env, "GIT_TERMINAL_PROMPT")
	require.Equal(t, "0", value)
	value, _ = envValue(env, "GCM_INTERACTIVE")
	require.Equal(t, "never", value)
	for _, dropped := range []string{"GIT_DIR", "GIT_ALLOW_PROTOCOL", "GIT_TEMPLATE_DIR", "GIT_TRACE", "GIT_CURL_VERBOSE"} {
		_, set := envValue(env, dropped)
		require.False(t, set, dropped)
	}
	for _, off := range []string{"GIT_TRACE2", "GIT_TRACE2_EVENT", "GIT_TRACE2_PERF"} {
		value, _ = envValue(env, off)
		require.Equal(t, "0", value, "%s is off, overriding a trace2 target in the host's configuration", off)
	}
	value, _ = envValue(gitSourcePrefetchEnv([]string{"GCM_INTERACTIVE=always"}), "GCM_INTERACTIVE")
	require.Equal(t, "always", value, "a host's own choice is kept")
	value, _ = envValue(env, "GIT_CONFIG_KEY_0")
	require.True(t, strings.HasPrefix(value, "url.https://token@"), "the host's own entry is kept first")
	require.Equal(t, map[string]string{
		"url.https://token@git.example.com/.insteadOf": "https://git.example.com/",
		"maintenance.auto": "false", "gc.auto": "0",
	}, gitConfigSettings(env))
}

func TestGitCommandReportsAMissingGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := runGitCommand(context.Background(), nil, "version")
	require.ErrorIs(t, err, errGitMissing)
}

// TestGitCommandOutputHasNoURLCredentials guards the error the prefetch returns,
// which is logged verbatim: a rewritten remote carrying a token must not reach it.
func TestGitCommandOutputHasNoURLCredentials(t *testing.T) {
	_, err := runGitCommand(context.Background(), os.Environ(), "--git-dir="+t.TempDir(), "fetch", "https://x-access-token:not-a-real-token@git.invalid/team/lib.git")
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
