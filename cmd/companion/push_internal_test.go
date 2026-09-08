package companion

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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
