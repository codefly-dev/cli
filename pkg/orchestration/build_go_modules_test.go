package orchestration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

type goCall struct {
	dir  string
	env  []string
	args []string
}

// recordingGo stands in for the go tool: it records each call and, for a
// download, populates the module cache the environment names.
func recordingGo(calls *[]goCall, fail error) func(context.Context, string, []string, ...string) error {
	return func(_ context.Context, dir string, env []string, args ...string) error {
		*calls = append(*calls, goCall{dir: dir, env: env, args: args})
		if fail != nil {
			return fail
		}
		for _, entry := range env {
			if cache, ok := strings.CutPrefix(entry, "GOMODCACHE="); ok && args[0] == "mod" {
				// What the go tool leaves: the proxy tree, the extracted
				// source and a version control clone.
				for _, dir := range []string{
					filepath.Join(cache, "cache", "download", "example.com", "lib", "@v"),
					filepath.Join(cache, "example.com", "lib@v1.0.0"),
					filepath.Join(cache, "cache", "vcs", "0123abcd"),
				} {
					if err := os.MkdirAll(dir, 0o755); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
}

func goModule(t *testing.T, dir string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/app\n\ngo 1.22\n"), 0o644))
	return dir
}

func envValue(env []string, key string) (string, bool) {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, key+"="); ok {
			return value, true
		}
	}
	return "", false
}

// TestGoModulePrefetchFetchesOnceIntoItsOwnCache pins what a build receives: the
// cache/download tree of a module cache private to the prefetch, fetched with the
// same two reads the build makes, once per module root however many recipes
// declare it, and pruned to that tree.
func TestGoModulePrefetchFetchesOnceIntoItsOwnCache(t *testing.T) {
	var calls []goCall
	prefetch := newGoModulePrefetch()
	prefetch.run = recordingGo(&calls, nil)
	t.Cleanup(func() { require.NoError(t, prefetch.Close()) })

	module := goModule(t, filepath.Join(t.TempDir(), "code"))
	proxy, err := prefetch.fetch(context.Background(), module)
	require.NoError(t, err)
	require.DirExists(t, filepath.Join(proxy, "example.com", "lib", "@v"))
	require.Equal(t, "download", filepath.Base(proxy))
	cache := filepath.Dir(filepath.Dir(proxy))
	require.NoDirExists(t, filepath.Join(cache, "example.com"), "extracted sources are pruned")
	require.NoDirExists(t, filepath.Join(cache, "cache", "vcs"), "version control clones are pruned")

	require.Len(t, calls, 2)
	require.Equal(t, []string{"mod", "download"}, calls[0].args)
	require.Equal(t, []string{"list", "-m", "-json", "all"}, calls[1].args)
	for _, call := range calls {
		require.Equal(t, module, call.dir)
		cache, ok := envValue(call.env, "GOMODCACHE")
		require.True(t, ok)
		require.Equal(t, filepath.Dir(filepath.Dir(proxy)), cache)
	}

	again, err := prefetch.fetch(context.Background(), module)
	require.NoError(t, err)
	require.Equal(t, proxy, again)
	require.Len(t, calls, 2, "a module root is fetched once per flow")

	root := prefetch.root
	require.NoError(t, prefetch.Close())
	require.NoDirExists(t, root)
}

// TestGoPrefetchEnvKeepsHowTheHostReachesModules pins the credential boundary:
// everything that decides how a module is reached stays the host's, and only
// where it is cached, how go.sum is treated and workspace mode are the
// prefetch's.
func TestGoPrefetchEnvKeepsHowTheHostReachesModules(t *testing.T) {
	env := goPrefetchEnv([]string{
		"GOPRIVATE=github.com/example-org/*",
		"GOPROXY=https://proxy.example.com",
		"NETRC=/home/dev/.netrc",
		"GOMODCACHE=/home/dev/go/pkg/mod",
		"GOFLAGS=-mod=vendor",
		"GOWORK=/home/dev/go.work",
	}, "/prefetch/0")
	for key, want := range map[string]string{
		"GOPRIVATE":  "github.com/example-org/*",
		"GOPROXY":    "https://proxy.example.com",
		"NETRC":      "/home/dev/.netrc",
		"GOMODCACHE": "/prefetch/0",
		"GOFLAGS":    "-mod=readonly -modcacherw",
		"GOWORK":     "off",
	} {
		value, ok := envValue(env, key)
		require.True(t, ok, key)
		require.Equal(t, want, value, key)
	}
	require.Len(t, env, 6, "an overridden key appears once")
}

func TestGoModulePrefetchRefusesARootWithoutGoMod(t *testing.T) {
	var calls []goCall
	prefetch := newGoModulePrefetch()
	prefetch.run = recordingGo(&calls, nil)
	_, err := prefetch.fetch(context.Background(), t.TempDir())
	require.ErrorContains(t, err, "no go.mod")
	require.Empty(t, calls)
}

func TestGoModulePrefetchReportsTheGoFailure(t *testing.T) {
	var calls []goCall
	prefetch := newGoModulePrefetch()
	prefetch.run = recordingGo(&calls, errors.New("go mod download: exit status 1"))
	t.Cleanup(func() { _ = prefetch.Close() })
	_, err := prefetch.fetch(context.Background(), goModule(t, t.TempDir()))
	require.ErrorContains(t, err, "cannot download the Go modules")
	require.ErrorContains(t, err, "exit status 1")
}

// TestProxiesForResolvesRootsAgainstTheContext pins the mapping a build is
// given: one proxy per declared build-context name, from the module root the
// recipe declares relative to its build context, and a root outside the
// context refused before anything is fetched.
func TestProxiesForResolvesRootsAgainstTheContext(t *testing.T) {
	var calls []goCall
	prefetch := newGoModulePrefetch()
	prefetch.run = recordingGo(&calls, nil)
	t.Cleanup(func() { _ = prefetch.Close() })

	contextDir := t.TempDir()
	goModule(t, filepath.Join(contextDir, "code"))
	goModule(t, filepath.Join(contextDir, "tools"))
	recipe := &builderv0.DockerBuildRecipe{Name: "app", GoModuleDownloads: []*builderv0.GoModuleDownload{
		{ModuleRoot: "code", ProxyContext: "gomodproxy"},
		{ModuleRoot: "tools", ProxyContext: "toolsproxy"},
	}}
	proxies, err := prefetch.proxiesFor(context.Background(), contextDir, recipe)
	require.NoError(t, err)
	require.Len(t, proxies, 2)
	require.Equal(t, proxies["gomodproxy"], proxies["toolsproxy"], "the flow's module roots share one cache")
	require.True(t, slices.ContainsFunc(calls, func(call goCall) bool { return call.dir == filepath.Join(contextDir, "tools") }))

	none, err := prefetch.proxiesFor(context.Background(), contextDir, &builderv0.DockerBuildRecipe{Name: "plain"})
	require.NoError(t, err)
	require.Nil(t, none)

	calls = nil
	_, err = prefetch.proxiesFor(context.Background(), contextDir, &builderv0.DockerBuildRecipe{Name: "escape", GoModuleDownloads: []*builderv0.GoModuleDownload{
		{ModuleRoot: "../elsewhere", ProxyContext: "gomodproxy"},
	}})
	require.ErrorContains(t, err, "outside its build context")
	require.Empty(t, calls)
}

// TestGoCommandOutputHasNoURLCredentials guards the error the prefetch returns,
// which is logged verbatim: a git remote rewritten with a CI token must not
// reach it.
func TestGoCommandOutputHasNoURLCredentials(t *testing.T) {
	redacted := redactURLCredentials("fatal: could not read from https://x-access-token:not-a-real-token@github.com/example-org/lib.git/")
	require.NotContains(t, redacted, "not-a-real-token")
	require.Contains(t, redacted, "https://***@github.com/example-org/lib.git/")
}

// TestAGoModuleRecipeIsNotBuiltWithoutItsProxies pins the refusal that keeps a
// declared download from silently falling back to a fetch inside the build.
func TestAGoModuleRecipeIsNotBuiltWithoutItsProxies(t *testing.T) {
	recipe := &builderv0.DockerBuildRecipe{Name: "app", GoModuleDownloads: []*builderv0.GoModuleDownload{{ModuleRoot: "code", ProxyContext: "gomodproxy"}}}
	require.True(t, recipeMissesGoModuleProxies(recipe, nil))
	require.True(t, recipeMissesGoModuleProxies(recipe, map[string]string{"other": "/p"}))
	require.False(t, recipeMissesGoModuleProxies(recipe, map[string]string{"gomodproxy": "/p"}))
	require.False(t, recipeMissesGoModuleProxies(&builderv0.DockerBuildRecipe{Name: "plain"}, nil))
}
