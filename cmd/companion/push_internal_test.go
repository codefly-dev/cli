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
	err := pushImage("proto", "ghcr.io/codefly-dev/proto:0.0.13")
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
	err := pushImage("proto", "ghcr.io/codefly-dev/proto:0.0.13")
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
	err := pushImage("proto", "ghcr.io/codefly-dev/proto:0.0.13")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not publicly pullable")
	require.Contains(t, err.Error(), "https://github.com/orgs/codefly-dev/packages/container/proto/settings")
}

// TestPushImage_FailsWhenPushedButPrivate_DockerHub is the case the original
// PR's tests never exercised: Companion.Tag() still produces a Docker Hub
// tag (codeflydev/<name>:<version>), not a ghcr.io one, so the "make it
// public" hint must not point at GitHub Packages for this — the package
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
	err := pushImage("proto", "codeflydev/proto:0.0.13")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not publicly pullable")
	require.Contains(t, err.Error(), "https://hub.docker.com/repository/docker/codeflydev/proto/general")
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

	err := pushImage("proto", "ghcr.io/codefly-dev/proto:0.0.13")
	require.NoError(t, err)

	got, err := os.ReadFile(countPath)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%d", pushVerifyAttempts), strings.TrimSpace(string(got)),
		"expected the check to succeed only on the last configured attempt")
}

func TestRegistryHost(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/codefly-dev/proto:0.0.13": "ghcr.io",
		"codeflydev/proto:0.0.13":          "docker.io",
		"localhost:5000/proto:0.0.13":      "localhost:5000",
	}
	for tag, want := range cases {
		require.Equal(t, want, registryHost(tag), "registryHost(%q)", tag)
	}
}
