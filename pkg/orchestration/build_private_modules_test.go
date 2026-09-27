package orchestration

import (
	"slices"
	"strings"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

func envOf(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

// TestResolvePrivateModuleBuildIsEmptyWithoutHostInputs pins the reproducible
// default: a host with no GOPRIVATE adds nothing to the build.
func TestResolvePrivateModuleBuildIsEmptyWithoutHostInputs(t *testing.T) {
	build := resolvePrivateModuleBuild(envOf(nil))
	require.Equal(t, privateModuleBuild{}, build)
	require.Empty(t, build.buildxArgs(nil))
}

func TestResolvePrivateModuleBuildTakesGoPrivateFromTheHost(t *testing.T) {
	build := resolvePrivateModuleBuild(envOf(map[string]string{"GOPRIVATE": " github.com/example-org/* "}))
	require.Equal(t, "github.com/example-org/*", build.GoPrivate)
}

// TestResolvePrivateModuleBuildNeverMountsACredential pins the removal of the
// build-time credential: whatever netrc the host environment names, no build
// argv carries a secret, because modules reach a build only as a prefetched
// proxy.
func TestResolvePrivateModuleBuildNeverMountsACredential(t *testing.T) {
	build := resolvePrivateModuleBuild(envOf(map[string]string{
		"GOPRIVATE":           "github.com/example-org/*",
		"CODEFLY_BUILD_NETRC": "/run/netrc",
		"NETRC":               "/run/netrc",
	}))
	args := strings.Join(build.withProxies(map[string]string{"gomodproxy": "/prefetch/0/cache/download"}).buildxArgs(nil), " ")
	require.NotContains(t, args, "--secret")
	require.NotContains(t, args, "netrc")
}

// TestPrivateModuleBuildxArgsPassGoPrivateAndTheProxies pins the flags the
// recipe contract depends on: GOPRIVATE is a build-arg the Dockerfile's `ARG
// GOPRIVATE` receives, unless the recipe declares its own, and each prefetched
// proxy is a named build context, in a stable order.
func TestPrivateModuleBuildxArgsPassGoPrivateAndTheProxies(t *testing.T) {
	build := privateModuleBuild{GoPrivate: "github.com/example-org/*"}.withProxies(map[string]string{
		"toolsproxy": "/prefetch/1/cache/download",
		"gomodproxy": "/prefetch/0/cache/download",
	})
	require.Equal(t, []string{
		"--build-arg", "GOPRIVATE=github.com/example-org/*",
		"--build-context", "gomodproxy=/prefetch/0/cache/download",
		"--build-context", "toolsproxy=/prefetch/1/cache/download",
	}, build.buildxArgs(nil))

	declared := build.buildxArgs(map[string]string{"GOPRIVATE": "git.example.com/*"})
	require.NotContains(t, declared, "GOPRIVATE=github.com/example-org/*")
	require.Equal(t, []string{"--build-arg", "GOPRIVATE=git.example.com"}, privateModuleBuild{GoPrivate: "git.example.com"}.buildxArgs(nil))
}

// TestCachedBuildxArgsKeepPrivateModuleFlagsBeforeTheContext asserts the flags
// land inside the buildx argv — after the recipe's own flags, before the
// positional context — for both a plain and a cached build.
func TestCachedBuildxArgsKeepPrivateModuleFlagsBeforeTheContext(t *testing.T) {
	private := privateModuleBuild{GoPrivate: "github.com/example-org/*"}.withProxies(map[string]string{"gomodproxy": "/prefetch/0/cache/download"})
	recipe := &builderv0.DockerBuildRecipe{Image: "repo/app:v1", Platforms: []string{"linux/amd64"}, BuildArgs: map[string]string{"VERSION": "1"}}

	for _, cache := range []*builderv0.BuildCacheOptions{nil, {Backend: "registry", Scope: "s", Imports: []string{"ghcr.io/org/cache"}}} {
		args, err := cachedBuildxArgs(recipe, "/svc/builder/Dockerfile", "/svc", false, false, "", "", cache, private)
		require.NoError(t, err)
		require.Equal(t, "/svc", args[len(args)-1])
		proxy := slices.Index(args, "gomodproxy=/prefetch/0/cache/download")
		require.Positive(t, proxy)
		require.Equal(t, "--build-context", args[proxy-1])
		goPrivate := slices.Index(args, "GOPRIVATE=github.com/example-org/*")
		require.Positive(t, goPrivate)
		require.Equal(t, "--build-arg", args[goPrivate-1])
		require.Less(t, slices.Index(args, "VERSION=1"), goPrivate)
		require.Less(t, proxy, slices.Index(args, "--progress"))
		require.NotContains(t, args, "--secret")
	}
}
