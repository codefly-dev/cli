package companion

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// withFastPushVerifyRetry shrinks the post-push anonymous-check retry delay
// for the duration of a test, so tests that exhaust the retries don't spend
// real wall-clock time waiting on it.
func withFastPushVerifyRetry(t *testing.T) {
	t.Helper()
	origDelay := pushVerifyDelay
	pushVerifyDelay = time.Millisecond
	t.Cleanup(func() { pushVerifyDelay = origDelay })
}

func TestPushImage_DeniedPrintsLoginHint(t *testing.T) {
	writeFakeDocker(t, `
echo "denied: requested access to the resource is denied" 1>&2
exit 1
`)
	_, err := pushImage("ghcr.io/codefly-dev/proto:0.0.13")
	require.Error(t, err)
	require.Contains(t, err.Error(), "docker login ghcr.io -u <user> -p $(gh auth token)")
}

func TestPushImage_SucceedsWhenAnonymouslyPullable(t *testing.T) {
	writeFakeDocker(t, `
case "$1" in
  push)
    echo "pushed"
    exit 0
    ;;
  manifest)
    echo '{}'
    exit 0
    ;;
esac
`)
	_, err := pushImage("ghcr.io/codefly-dev/proto:0.0.13")
	require.NoError(t, err)
}

func TestPushImage_FailsWhenPushedButPrivate(t *testing.T) {
	withFastPushVerifyRetry(t)
	writeFakeDocker(t, `
case "$1" in
  push)
    echo "pushed"
    exit 0
    ;;
  manifest)
    echo "denied: requested access to the resource is denied" 1>&2
    exit 1
    ;;
esac
`)
	_, err := pushImage("ghcr.io/codefly-dev/proto:0.0.13")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not publicly pullable")
	require.Contains(t, err.Error(), "https://github.com/orgs/codefly-dev/packages/container/proto/settings")
}

// TestPushImage_FailsWhenPushedButPrivate_DockerHub covers a non-ghcr.io tag
// directly (pushImage's registry hint is registry-agnostic, even though
// Companion.Tag() only ever produces ghcr.io ones now): the "make it public"
// hint must not point at GitHub Packages for a Docker Hub tag — the package
// doesn't exist there.
func TestPushImage_FailsWhenPushedButPrivate_DockerHub(t *testing.T) {
	withFastPushVerifyRetry(t)
	writeFakeDocker(t, `
case "$1" in
  push)
    echo "pushed"
    exit 0
    ;;
  manifest)
    echo "denied: requested access to the resource is denied" 1>&2
    exit 1
    ;;
esac
`)
	_, err := pushImage("acme/proto:0.0.13")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not publicly pullable")
	require.Contains(t, err.Error(), "https://hub.docker.com/repository/docker/acme/proto/general")
	require.NotContains(t, err.Error(), "github.com/orgs")
}

// TestPushImage_RetriesTransientAnonymousCheckFailure covers the failure
// mode identified in review: the post-push anonymous check can see a
// just-pushed tag as not-yet-readable (registry propagation, or the
// Docker-Hub anonymous-auth quirk that used to require `docker login`
// before any check ran) and must not report that as "not publicly
// pullable" on the very first failed read.
func TestPushImage_RetriesTransientAnonymousCheckFailure(t *testing.T) {
	withFastPushVerifyRetry(t)
	countPath := filepath.Join(t.TempDir(), "manifest-inspect-count")
	writeFakeDocker(t, fmt.Sprintf(`
case "$1" in
  push)
    echo "pushed"
    exit 0
    ;;
  manifest)
    n=0
    if [ -f %q ]; then n=$(cat %q); fi
    n=$((n+1))
    echo "$n" > %q
    if [ "$n" -lt %d ]; then
      echo "denied: requested access to the resource is denied" 1>&2
      exit 1
    fi
    echo '{}'
    exit 0
    ;;
esac
`, countPath, countPath, countPath, pushVerifyAttempts))

	_, err := pushImage("ghcr.io/codefly-dev/proto:0.0.13")
	require.NoError(t, err)

	got, err := os.ReadFile(countPath)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%d", pushVerifyAttempts), strings.TrimSpace(string(got)),
		"expected the check to succeed only on the last configured attempt")
}

func TestRegistryHost(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/codefly-dev/proto:0.0.13": "ghcr.io",
		"acme/proto:0.0.13":                "docker.io",
		"localhost:5000/proto:0.0.13":      "localhost:5000",
	}
	for tag, want := range cases {
		require.Equal(t, want, registryHost(tag), "registryHost(%q)", tag)
	}
}

// TestPushImage_DeniedOnDockerHubDoesNotSuggestGitHubToken pins the credential
// to the registry. `gh auth token` mints a GitHub token, which Docker Hub will
// never accept, so printing it for a docker.io denial sends the operator into
// a login that cannot succeed.
func TestPushImage_DeniedOnDockerHubDoesNotSuggestGitHubToken(t *testing.T) {
	writeFakeDocker(t, `
echo "denied: requested access to the resource is denied" 1>&2
exit 1
`)
	_, err := pushImage("acme/proto:0.0.13")
	require.Error(t, err)
	require.Contains(t, err.Error(), "docker login docker.io")
	require.NotContains(t, err.Error(), "gh auth token")
}

// TestBuildTargets_ContinuesPushingAfterAFailure pins the fleet behaviour: a
// companion whose package is private (the default state of a brand-new ghcr
// package, fixable only through the UI) must not strand every companion
// queued behind it. The run still fails, but every target is attempted and
// all failures are reported together.
func TestBuildTargets_ContinuesPushingAfterAFailure(t *testing.T) {
	withFastPushVerifyRetry(t)
	root := t.TempDir()
	writeSiblingCLI(t, root)
	writeManifest(t, root, baseCompanionName, "0.0.1", true, false)
	writeManifest(t, root, "execution", "0.0.2", true, false)

	pushLog := filepath.Join(t.TempDir(), "pushed.txt")
	writeFakeDocker(t, fmt.Sprintf(`
case "$1" in
  build)
    exit 0
    ;;
  push)
    echo "$2" >> %q
    echo "pushed"
    exit 0
    ;;
  manifest)
    echo "denied: requested access to the resource is denied" 1>&2
    exit 1
    ;;
esac
exit 0
`, pushLog))

	base, err := LoadCompanion(filepath.Join(root, "companions", baseCompanionName))
	require.NoError(t, err)
	execution, err := LoadCompanion(filepath.Join(root, "companions", "execution"))
	require.NoError(t, err)

	_, err = buildTargets(root, []*Companion{base, execution}, BuildOptions{Push: true})
	require.Error(t, err)
	require.Contains(t, err.Error(), baseCompanionName)
	require.Contains(t, err.Error(), "execution", "the second companion must still be attempted and reported")

	pushed, readErr := os.ReadFile(pushLog)
	require.NoError(t, readErr)
	require.Contains(t, string(pushed), base.Tag())
	require.Contains(t, string(pushed), execution.Tag(),
		"a private package on the first companion must not abort the rest of the fleet")
}

func TestRepoPath(t *testing.T) {
	cases := map[string]string{
		"acme/proto:0.0.13":                       "acme/proto",
		"acme/proto@sha256:deadbeef":              "acme/proto",
		"acme/proto":                              "acme/proto",
		"localhost:5000/proto:0.0.13":             "localhost:5000/proto",
		"localhost:5000/proto":                    "localhost:5000/proto",
		"ghcr.io/codefly-dev/proto:0.0.13":        "ghcr.io/codefly-dev/proto",
		"ghcr.io/codefly-dev/proto@sha256:abc123": "ghcr.io/codefly-dev/proto",
	}
	for ref, want := range cases {
		require.Equal(t, want, repoPath(ref), "repoPath(%q)", ref)
	}
}

// TestRegistryPrivacyHint covers the two ways this hint used to be wrong: it
// took the org/namespace from a caller-supplied name rather than the reference
// (so any non-codefly-dev org got a codefly-dev URL), and it split the tag on
// the last colon (so a digest reference produced ".../proto@sha256/general").
func TestRegistryPrivacyHint(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/codefly-dev/proto:0.0.13": "https://github.com/orgs/codefly-dev/packages/container/proto/settings",
		"ghcr.io/acme/widget:1.0":          "https://github.com/orgs/acme/packages/container/widget/settings",
		"ghcr.io/acme/widget@sha256:abc":   "https://github.com/orgs/acme/packages/container/widget/settings",
		"acme/proto:0.0.13":                "https://hub.docker.com/repository/docker/acme/proto/general",
		"acme/proto@sha256:deadbeef":       "https://hub.docker.com/repository/docker/acme/proto/general",
	}
	for tag, want := range cases {
		require.Contains(t, registryPrivacyHint(tag), want, "registryPrivacyHint(%q)", tag)
	}

	// A reference with no resolvable org/namespace must fall back rather than
	// emit a malformed URL.
	for _, tag := range []string{"ghcr.io/proto:1.0", "localhost:5000/proto:1.0"} {
		hint := registryPrivacyHint(tag)
		require.Contains(t, hint, "visibility settings in its registry", "registryPrivacyHint(%q)", tag)
		require.NotContains(t, hint, "https://", "registryPrivacyHint(%q) must not invent a URL", tag)
	}
}
