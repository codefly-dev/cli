//go:build integration

package gitops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"io"
	"runtime/debug"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/semver"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
)

const (
	// coreModule is the module whose types the paired runner decodes a
	// module manifest into.
	coreModule = "github.com/codefly-dev/core"
	// companionBinary is where the codefly companion bakes the CLI.
	companionBinary = "bin/codefly"
)

// The paired delivery Job runs the codefly baked into pairedDeliveryImage, and
// that binary decodes the module manifest this CLI renders STRICTLY: a field
// its core does not know fails the Job with "field … not found", which is how
// a manifest carrying authority.source_governance failed on a cell whose image
// embedded core v0.19.17 under a CLI built with v0.19.27. Nothing else ties the
// two: the image is pinned by digest here and go.mod moves on its own. So this
// reads the core version out of the binary in every platform of the pinned
// index — the bytes a cell runs, not a record of them — and fails when it is
// older than the core this CLI is built with.
func TestThePairedDeliveryImageDecodesWhatThisCLIRenders(t *testing.T) {
	want := cliCoreVersion(t)
	repository, digest, found := strings.Cut(pairedDeliveryImage, "@")
	require.True(t, found, "pairedDeliveryImage %q is not pinned by digest", pairedDeliveryImage)
	repo, err := remote.NewRepository(repository)
	require.NoError(t, err)

	index := fetchJSON[ocispec.Index](t, repo, digest)
	measured := map[string]string{}
	for _, manifest := range index.Manifests {
		// buildx adds attestation manifests with an unknown platform.
		if manifest.Platform == nil || manifest.Platform.OS != "linux" {
			continue
		}
		info := companionBuildInfo(t, repo, manifest)
		require.Equal(t, "github.com/codefly-dev/cli", info.Path, "%s %s does not bake the codefly CLI", pairedDeliveryImage, manifest.Platform.Architecture)
		measured[manifest.Platform.Architecture] = dependencyVersion(t, info, coreModule)
	}
	require.ElementsMatch(t, []string{archAMD64, archARM64}, keys(measured), "a delivery Job lands on either architecture")
	for arch, got := range measured {
		require.GreaterOrEqual(t, semver.Compare(got, want), 0,
			"the codefly in %s (linux/%s) embeds %s %s, older than the %s this CLI renders with: "+
				"publish a codefly companion from this checkout (core docs/runbooks/publish-companions.md) and pin its index digest as pairedDeliveryImage",
			pairedDeliveryImage, arch, coreModule, got, want)
	}
}

const (
	archAMD64 = "amd64"
	archARM64 = "arm64"
)

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// cliCoreVersion is the core this test binary — so this CLI — is built with.
func cliCoreVersion(t *testing.T) string {
	t.Helper()
	info, ok := debug.ReadBuildInfo()
	require.True(t, ok, "the test binary carries no build information")
	return dependencyVersion(t, info, coreModule)
}

func dependencyVersion(t *testing.T, info *debug.BuildInfo, path string) string {
	t.Helper()
	for _, dep := range info.Deps {
		if dep.Path != path {
			continue
		}
		if dep.Replace != nil {
			dep = dep.Replace
		}
		require.True(t, semver.IsValid(dep.Version), "%s is built with %s at %q, which is not a version", info.Path, path, dep.Version)
		return dep.Version
	}
	t.Fatalf("%s is not built with %s", info.Path, path)
	return ""
}

func fetchJSON[T any](t *testing.T, repo *remote.Repository, reference string) T {
	t.Helper()
	descriptor, err := repo.Resolve(t.Context(), reference)
	require.NoError(t, err)
	data, err := content.FetchAll(t.Context(), repo, descriptor)
	require.NoError(t, err)
	var out T
	require.NoError(t, json.Unmarshal(data, &out))
	return out
}

// companionBuildInfo reads the build information of the codefly binary in one
// platform's image: the topmost layer that holds it is the one a container sees.
func companionBuildInfo(t *testing.T, repo *remote.Repository, platform ocispec.Descriptor) *debug.BuildInfo {
	t.Helper()
	image := fetchJSON[ocispec.Manifest](t, repo, platform.Digest.String())
	for i := len(image.Layers) - 1; i >= 0; i-- {
		binary, err := fileInLayer(t.Context(), repo, image.Layers[i], companionBinary)
		if errors.Is(err, errNotInLayer) {
			continue
		}
		require.NoError(t, err)
		info, err := buildinfo.Read(bytes.NewReader(binary))
		require.NoError(t, err)
		return info
	}
	t.Fatalf("no layer of %s (%s) holds %s", pairedDeliveryImage, platform.Platform.Architecture, companionBinary)
	return nil
}

var errNotInLayer = errors.New("not in layer")

func fileInLayer(ctx context.Context, repo *remote.Repository, layer ocispec.Descriptor, name string) ([]byte, error) {
	if layer.MediaType != ocispec.MediaTypeImageLayerGzip && layer.MediaType != "application/vnd.docker.image.rootfs.diff.tar.gzip" {
		return nil, errors.New("layer " + layer.Digest.String() + " is " + layer.MediaType + ", not a gzip tar")
	}
	blob, err := repo.Fetch(ctx, layer)
	if err != nil {
		return nil, err
	}
	defer func() { _ = blob.Close() }()
	decompressed, err := gzip.NewReader(blob)
	if err != nil {
		return nil, err
	}
	archive := tar.NewReader(decompressed)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil, errNotInLayer
		}
		if err != nil {
			return nil, err
		}
		if strings.TrimPrefix(header.Name, "./") == name && header.Typeflag == tar.TypeReg {
			return io.ReadAll(archive)
		}
	}
}
