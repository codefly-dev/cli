package imagedigest

import (
	"strings"
	"testing"

	"github.com/distribution/reference"
	"github.com/stretchr/testify/require"
)

var (
	digestA = "sha256:" + strings.Repeat("a", 64)
	digestB = "sha256:" + strings.Repeat("b", 64)
)

func named(t *testing.T, image string) reference.Named {
	t.Helper()
	n, err := reference.ParseNormalizedNamed(image)
	require.NoError(t, err)
	return n
}

// Only a digest in the image's own repository is the image's: the same image
// tagged under another name carries that name's digest, not this one's.
func TestOnlyTheImagesOwnRepositoryDigestCounts(t *testing.T) {
	image := named(t, "ghcr.io/codefly-dev/proto:0.0.25")
	require.Equal(t, []string{digestA}, digestsIn(image, []string{"ghcr.io/codefly-dev/proto@" + digestA, "ghcr.io/codefly-dev/proto@" + digestA}))
	require.Empty(t, digestsIn(image, []string{"ghcr.io/other/proto@" + digestA, "mirror.io/codefly-dev/proto@" + digestB}))
	require.Empty(t, digestsIn(image, []string{"ghcr.io/codefly-dev/proto@sha256:short", "ghcr.io/codefly-dev/proto"}))
	// Names compare as docker normalises them.
	require.Equal(t, []string{digestA}, digestsIn(named(t, "golang:1.27"), []string{"docker.io/library/golang@" + digestA}))
	require.Len(t, digestsIn(image, []string{"ghcr.io/codefly-dev/proto@" + digestA, "ghcr.io/codefly-dev/proto@" + digestB}), 2)
}

// A digest is published only when the registry serves it and the daemon's
// image in the same repository is it. On the containerd image store a build
// that was never pushed has a repository digest of its own (equal to its image
// ID): that digest is not the registry's and must never be taken for it.
func TestPublishedNeedsTheRegistrysDigestOnTheDaemonsImage(t *testing.T) {
	image := "ghcr.io/codefly-dev/proto:0.0.25"
	got, err := published(image, named(t, image), digestA, []string{"ghcr.io/codefly-dev/proto@" + digestA})
	require.NoError(t, err)
	require.Equal(t, digestA, got)

	for name, references := range map[string][]string{
		"a never-pushed containerd build": {"ghcr.io/codefly-dev/proto@" + digestB},
		"the digest under another name":   {"ghcr.io/other/proto@" + digestA},
		"no repository digest":            nil,
	} {
		_, err := published(image, named(t, image), digestA, references)
		require.Error(t, err, name)
	}
}

func TestRegistryReferencesResolveAsDockerDoes(t *testing.T) {
	repository, ref, err := Repository("golang:1.27-bookworm")
	require.NoError(t, err)
	require.Equal(t, "docker.io/library/golang", repository.Reference.Registry+"/"+repository.Reference.Repository)
	require.Equal(t, "1.27-bookworm", ref)
	require.False(t, repository.PlainHTTP)

	repository, ref, err = Repository("localhost:5000/app@sha256:" + strings.Repeat("b", 64))
	require.NoError(t, err)
	require.Equal(t, "sha256:"+strings.Repeat("b", 64), ref)
	require.True(t, repository.PlainHTTP, "a loopback registry is plain HTTP, as docker reaches it")

	_, ref, err = Repository("ghcr.io/org/app")
	require.NoError(t, err)
	require.Equal(t, "latest", ref)
}
