package orchestration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	require.Len(t, env, 6+1+2*len(gitPrefetchConfig), "an overridden key appears once")
}

// TestGoPrefetchEnvStopsGitMaintainingTheCache pins the one thing the prefetch
// imposes on git: the automatic maintenance a `git fetch` detaches must not run
// in this cache, because it rewrites the shallow clone the next fetch of the
// same repository is deepening — which is how a pseudo-version a render only
// had to download failed the render (codefly-dev/cli#886).
func TestGoPrefetchEnvStopsGitMaintainingTheCache(t *testing.T) {
	settings := gitConfigSettings(goPrefetchEnv([]string{"GOPRIVATE=github.com/example-org/*"}, "/prefetch/0"))
	require.Equal(t, map[string]string{"maintenance.auto": "false", "gc.auto": "0"}, settings)
}

// TestGoPrefetchEnvKeepsTheHostsGitConfigEntries pins the boundary the settings
// above are added across: a host that already configures git through the
// environment keeps every entry it declared, and the prefetch's own are read
// after them — git applies them in index order, so the later one wins.
func TestGoPrefetchEnvKeepsTheHostsGitConfigEntries(t *testing.T) {
	env := goPrefetchEnv([]string{
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=url.https://token@github.com/.insteadOf",
		"GIT_CONFIG_VALUE_0=https://github.com/",
		"GIT_CONFIG_KEY_1=maintenance.auto",
		"GIT_CONFIG_VALUE_1=true",
		// Past the host's own count: git reads neither, and the prefetch's
		// entries take those indices.
		"GIT_CONFIG_KEY_2=core.editor",
		"GIT_CONFIG_VALUE_2=vi",
	}, "/prefetch/0")
	settings := gitConfigSettings(env)
	require.Equal(t, "https://github.com/", settings["url.https://token@github.com/.insteadOf"], "the host's rewrite survives")
	require.Equal(t, "false", settings["maintenance.auto"], "the prefetch's entry is read after the host's")
	require.Equal(t, "0", settings["gc.auto"])
	require.NotContains(t, settings, "core.editor", "an entry the host's own count excluded is not promoted")
	count, ok := envValue(env, "GIT_CONFIG_COUNT")
	require.True(t, ok)
	require.Equal(t, "4", count)
}

// gitConfigSettings reads an environment the way git does: GIT_CONFIG_COUNT
// entries, by index, each later setting of a key winning.
func gitConfigSettings(env []string) map[string]string {
	raw, ok := envValue(env, "GIT_CONFIG_COUNT")
	if !ok {
		return nil
	}
	count, err := strconv.Atoi(raw)
	if err != nil {
		return nil
	}
	settings := map[string]string{}
	for index := 0; index < count; index++ {
		key, hasKey := envValue(env, fmt.Sprintf("GIT_CONFIG_KEY_%d", index))
		value, hasValue := envValue(env, fmt.Sprintf("GIT_CONFIG_VALUE_%d", index))
		if !hasKey || !hasValue {
			continue
		}
		settings[key] = value
	}
	return settings
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
