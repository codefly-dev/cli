package builder

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// writeFakeDocker installs a fake `docker` executable at the front of PATH
// for the duration of the test. body is the shell script run for every
// invocation, regardless of subcommand.
func writeFakeDocker(t *testing.T, body string) {
	t.Helper()
	binDir := t.TempDir()
	script := "#!/bin/sh\n" + body
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "docker"), []byte(script), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

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
	err := PushImage("proto", "ghcr.io/codefly-dev/proto:0.0.13")
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
	err := PushImage("proto", "ghcr.io/codefly-dev/proto:0.0.13")
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
	err := PushImage("proto", "ghcr.io/codefly-dev/proto:0.0.13")
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
	err := PushImage("proto", "codeflydev/proto:0.0.13")
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

	err := PushImage("proto", "ghcr.io/codefly-dev/proto:0.0.13")
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
		require.Equal(t, want, RegistryHost(tag), "RegistryHost(%q)", tag)
	}
}

func TestAnonymousManifestInspect_RunsWithEmptyDockerConfig(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "record.txt")
	writeFakeDocker(t, fmt.Sprintf(`
{
  echo "argv: $@"
  echo "DOCKER_CONFIG=$DOCKER_CONFIG"
  cat "$DOCKER_CONFIG/config.json"
} >> %q
echo '{}'
exit 0
`, recordPath))

	ok, _, err := AnonymousManifestInspect("ghcr.io/codefly-dev/proto:0.0.13")
	require.NoError(t, err)
	require.True(t, ok)

	record, err := os.ReadFile(recordPath)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimRight(string(record), "\n"), "\n")
	require.Len(t, lines, 3, "record: %q", record)
	require.Equal(t, "argv: manifest inspect ghcr.io/codefly-dev/proto:0.0.13", lines[0])
	require.True(t, strings.HasPrefix(lines[1], "DOCKER_CONFIG="), "record: %q", record)
	require.NotEmpty(t, strings.TrimPrefix(lines[1], "DOCKER_CONFIG="), "DOCKER_CONFIG must be set to a real path")
	require.Equal(t, "{}", lines[2], "docker must run against an empty, credential-free config")
}

func TestIsManifestNotFound(t *testing.T) {
	for _, s := range []string{
		"no such manifest: codeflydev/proto:0.0.11",
		"manifest unknown",
		"MANIFEST UNKNOWN: manifest unknown", // case-insensitive
	} {
		require.True(t, IsManifestNotFound(s), "%q should be classified as missing", s)
	}
	// Anything that isn't an explicit manifest-not-found must be surfaced as
	// a real error, not silently treated as a missing tag — including
	// repository/auth failures that merely contain "not found".
	for _, s := range []string{
		"error during connect: dial tcp: i/o timeout",
		"unauthorized: authentication required",
		"denied: requested access to the resource is denied",
		"repository codeflydev/proto not found",
		"",
	} {
		require.False(t, IsManifestNotFound(s), "%q must not be classified as missing", s)
	}
}
