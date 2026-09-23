package orchestration

import (
	"os"
	"path/filepath"
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

func homeOf(dir string) func() (string, error) {
	return func() (string, error) { return dir, nil }
}

func writeNetrc(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("machine git.example.com login x-access-token password not-a-real-token\n"), 0o600))
	return path
}

// TestResolvePrivateModuleBuildIsEmptyWithoutHostInputs pins the reproducible
// default: a host with no GOPRIVATE and no netrc adds nothing to the build, so
// the recipe builds exactly as a consumer rebuilding it by hand would.
func TestResolvePrivateModuleBuildIsEmptyWithoutHostInputs(t *testing.T) {
	build, err := resolvePrivateModuleBuild(envOf(nil), homeOf(t.TempDir()))
	require.NoError(t, err)
	require.Equal(t, privateModuleBuild{}, build)
	require.Empty(t, build.buildxArgs(nil))
}

// TestResolvePrivateModuleBuildTakesGoPrivateFromTheHost asserts GOPRIVATE
// reaches the build the way the go tool reads it on the host — verbatim.
func TestResolvePrivateModuleBuildTakesGoPrivateFromTheHost(t *testing.T) {
	build, err := resolvePrivateModuleBuild(envOf(map[string]string{"GOPRIVATE": " github.com/example-org/*,git.example.com "}), homeOf(t.TempDir()))
	require.NoError(t, err)
	require.Equal(t, "github.com/example-org/*,git.example.com", build.GoPrivate)
	require.Empty(t, build.Netrc)
}

// TestResolvePrivateModuleBuildPrefersExplicitNetrcAndRequiresIt holds the CI
// contract: CODEFLY_BUILD_NETRC wins over every other source, and one that
// points nowhere fails the build up front instead of surfacing as git's
// "could not read Username" from inside BuildKit.
func TestResolvePrivateModuleBuildPrefersExplicitNetrcAndRequiresIt(t *testing.T) {
	home := t.TempDir()
	writeNetrc(t, home, ".netrc")
	explicit := writeNetrc(t, t.TempDir(), "ci-netrc")
	goNetrc := writeNetrc(t, t.TempDir(), "go-netrc")

	build, err := resolvePrivateModuleBuild(envOf(map[string]string{"CODEFLY_BUILD_NETRC": explicit, "NETRC": goNetrc}), homeOf(home))
	require.NoError(t, err)
	require.Equal(t, explicit, build.Netrc)

	_, err = resolvePrivateModuleBuild(envOf(map[string]string{"CODEFLY_BUILD_NETRC": filepath.Join(home, "missing")}), homeOf(home))
	require.ErrorContains(t, err, "CODEFLY_BUILD_NETRC")

	_, err = resolvePrivateModuleBuild(envOf(map[string]string{"CODEFLY_BUILD_NETRC": home}), homeOf(home))
	require.ErrorContains(t, err, "not a regular file")

	// buildx parses --secret as CSV; a comma would split the path into a bogus
	// second option, so it is refused here with a message naming the cause.
	comma := writeNetrc(t, t.TempDir(), "with,comma")
	_, err = resolvePrivateModuleBuild(envOf(map[string]string{"CODEFLY_BUILD_NETRC": comma}), homeOf(home))
	require.ErrorContains(t, err, "comma")
}

// TestResolvePrivateModuleBuildFallsBackLikeGitAndGo asserts the opportunistic
// sources behave as they do for the tools that read them: NETRC when it names a
// file, then $HOME/.netrc when present, and no credential — no error — when
// neither exists.
func TestResolvePrivateModuleBuildFallsBackLikeGitAndGo(t *testing.T) {
	home := t.TempDir()
	goNetrc := writeNetrc(t, t.TempDir(), "go-netrc")

	build, err := resolvePrivateModuleBuild(envOf(map[string]string{"NETRC": goNetrc}), homeOf(home))
	require.NoError(t, err)
	require.Equal(t, goNetrc, build.Netrc)

	homeNetrc := writeNetrc(t, home, ".netrc")
	build, err = resolvePrivateModuleBuild(envOf(map[string]string{"NETRC": filepath.Join(home, "absent")}), homeOf(home))
	require.NoError(t, err)
	require.Equal(t, homeNetrc, build.Netrc)

	build, err = resolvePrivateModuleBuild(envOf(nil), homeOf(t.TempDir()))
	require.NoError(t, err)
	require.Empty(t, build.Netrc)
}

// TestPrivateModuleBuildxArgsMountTheSecretAndPassGoPrivate pins the flags the
// agent recipe contract depends on: the secret is passed by id and source path
// only — its content never enters the argv — and GOPRIVATE is a build-arg the
// Dockerfile's `ARG GOPRIVATE` receives. A recipe that declares GOPRIVATE
// itself is not overridden by the host.
func TestPrivateModuleBuildxArgsMountTheSecretAndPassGoPrivate(t *testing.T) {
	netrc := writeNetrc(t, t.TempDir(), ".netrc")
	build := privateModuleBuild{GoPrivate: "github.com/example-org/*", Netrc: netrc}

	args := build.buildxArgs(nil)
	require.Equal(t, []string{"--build-arg", "GOPRIVATE=github.com/example-org/*", "--secret", "id=netrc,src=" + netrc}, args)
	require.NotContains(t, strings.Join(args, " "), "not-a-real-token")

	declared := build.buildxArgs(map[string]string{"GOPRIVATE": "git.example.com/*"})
	require.Equal(t, []string{"--secret", "id=netrc,src=" + netrc}, declared)

	require.Equal(t, []string{"--secret", "id=netrc,src=" + netrc}, privateModuleBuild{Netrc: netrc}.buildxArgs(nil))
	require.Equal(t, []string{"--build-arg", "GOPRIVATE=git.example.com"}, privateModuleBuild{GoPrivate: "git.example.com"}.buildxArgs(nil))
}

// TestCachedBuildxArgsKeepPrivateModuleFlagsBeforeTheContext asserts the flags
// land inside the buildx argv — after the recipe's own flags, before the
// positional context — for both a plain and a cached build.
func TestCachedBuildxArgsKeepPrivateModuleFlagsBeforeTheContext(t *testing.T) {
	netrc := writeNetrc(t, t.TempDir(), ".netrc")
	private := privateModuleBuild{GoPrivate: "github.com/example-org/*", Netrc: netrc}
	recipe := &builderv0.DockerBuildRecipe{Image: "repo/app:v1", Platforms: []string{"linux/amd64"}, BuildArgs: map[string]string{"VERSION": "1"}}

	for _, cache := range []*builderv0.BuildCacheOptions{nil, {Backend: "registry", Scope: "s", Imports: []string{"ghcr.io/org/cache"}}} {
		args, err := cachedBuildxArgs(recipe, "/svc/builder/Dockerfile", "/svc", false, false, "", "", cache, private)
		require.NoError(t, err)
		require.Equal(t, "/svc", args[len(args)-1])
		secret := slices.Index(args, "--secret")
		require.Positive(t, secret)
		require.Equal(t, "id=netrc,src="+netrc, args[secret+1])
		goPrivate := slices.Index(args, "GOPRIVATE=github.com/example-org/*")
		require.Positive(t, goPrivate)
		require.Equal(t, "--build-arg", args[goPrivate-1])
		require.Less(t, slices.Index(args, "VERSION=1"), goPrivate)
		require.Less(t, secret, slices.Index(args, "--progress"))
	}
}
