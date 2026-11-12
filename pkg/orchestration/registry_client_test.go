package orchestration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/codefly-dev/core/resources"
	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

// fakeRegistry serves one repository over plain HTTP on loopback, as a local
// registry does, and counts what it is asked.
type fakeRegistry struct {
	mu        sync.Mutex
	manifests map[string]served // by tag and by digest
	blobs     map[string][]byte
	gets      map[string]int
}

type served struct {
	mediaType string
	raw       []byte
}

func digestOfBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (registry *fakeRegistry) add(mediaType string, value any) ocispec.Descriptor {
	raw, _ := json.Marshal(value)
	digest := digestOfBytes(raw)
	registry.manifests[digest] = served{mediaType, raw}
	return ocispec.Descriptor{MediaType: mediaType, Digest: godigest.Digest(digest), Size: int64(len(raw))}
}

func (registry *fakeRegistry) blob(value any) ocispec.Descriptor {
	raw, _ := json.Marshal(value)
	digest := digestOfBytes(raw)
	registry.blobs[digest] = raw
	return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: godigest.Digest(digest), Size: int64(len(raw))}
}

func (registry *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.gets[r.Method+" "+r.URL.Path]++
	if kind, ref, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v2/app/"), "/"); ok {
		switch kind {
		case "manifests":
			if found, known := registry.manifests[ref]; known {
				w.Header().Set("Content-Type", found.mediaType)
				w.Header().Set("Docker-Content-Digest", digestOfBytes(found.raw))
				_, _ = w.Write(found.raw)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`))
			return
		case "blobs":
			if raw, known := registry.blobs[ref]; known {
				_, _ = w.Write(raw)
				return
			}
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

func imageWithLabels(registry *fakeRegistry, labels map[string]string, platform string) ocispec.Descriptor {
	config := registry.blob(map[string]any{"config": map[string]any{"Labels": labels}})
	manifest := registry.add(ocispec.MediaTypeImageManifest, ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, Config: config})
	if os, arch, ok := strings.Cut(platform, "/"); ok {
		manifest.Platform = &ocispec.Platform{OS: os, Architecture: arch}
	}
	return manifest
}

// One fetch of a tag answers both the manifest check and the labels check for
// the render, the labels are those every platform agrees on with the
// attestation manifest no platform, and a manifest digest's labels are read
// from the registry once ever.
func TestRegistryReadsShareOneFetchAndKeepLabelsByDigest(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	registry := &fakeRegistry{manifests: map[string]served{}, blobs: map[string][]byte{}, gets: map[string]int{}}
	key := "sha256:" + strings.Repeat("a", 64)
	amd := imageWithLabels(registry, map[string]string{buildInputsLabel: key, buildCodeLabel: "sha256:c", "unrelated": "x"}, "linux/amd64")
	arm := imageWithLabels(registry, map[string]string{buildInputsLabel: key, buildCodeLabel: "sha256:other"}, "linux/arm64")
	attestation := imageWithLabels(registry, nil, "unknown/unknown")
	attestation.Annotations = map[string]string{attestationReferenceType: attestationManifest}
	index := registry.add(ocispec.MediaTypeImageIndex, ocispec.Index{MediaType: ocispec.MediaTypeImageIndex, Manifests: []ocispec.Descriptor{amd, arm, attestation}})
	registry.manifests["1.0"] = registry.manifests[index.Digest.String()]
	server := httptest.NewServer(registry)
	defer server.Close()
	image := strings.TrimPrefix(server.URL, "http://") + "/app:1.0"

	ctx := withProbes(context.Background(), newProbeCache())
	digest, err := resolveImageManifestDigest(ctx, image, "")
	require.NoError(t, err)
	require.Equal(t, index.Digest.String(), digest, "the digest is over the bytes the registry served")
	labels, err := resolveImageBuildLabels(ctx, image, "")
	require.NoError(t, err)
	require.Equal(t, map[string]string{buildInputsLabel: key}, labels, "a label the platforms disagree on proves nothing")
	require.Equal(t, 1, registry.gets["GET /v2/app/manifests/1.0"], "the manifest and labels checks share one fetch")
	require.Equal(t, 1, registry.gets["GET /v2/app/blobs/"+registryConfigOf(t, registry, amd)])

	// Another render: the tag is read again, its digest's labels are not.
	labels, err = resolveImageBuildLabels(withProbes(context.Background(), newProbeCache()), image, "")
	require.NoError(t, err)
	require.Equal(t, map[string]string{buildInputsLabel: key}, labels)
	require.Equal(t, 2, registry.gets["GET /v2/app/manifests/1.0"], "a tag's resolution is never kept across renders")
	require.Equal(t, 1, registry.gets["GET /v2/app/blobs/"+registryConfigOf(t, registry, amd)], "a digest's labels are read once")

	_, err = resolveImageManifestDigest(ctx, strings.TrimPrefix(server.URL, "http://")+"/app:2.0", "")
	require.True(t, isImageNotFound(err), "a registry's not-found is the reason to build: %v", err)
}

// registryConfigOf is the configuration digest an image manifest names.
func registryConfigOf(t *testing.T, registry *fakeRegistry, manifest ocispec.Descriptor) string {
	t.Helper()
	var parsed ocispec.Manifest
	require.NoError(t, json.Unmarshal(registry.manifests[manifest.Digest.String()].raw, &parsed))
	return parsed.Config.Digest.String()
}

func TestRegistryReferencesResolveAsDockerDoes(t *testing.T) {
	repository, ref, err := registryRepository("golang:1.27-bookworm")
	require.NoError(t, err)
	require.Equal(t, "docker.io/library/golang", repository.Reference.Registry+"/"+repository.Reference.Repository)
	require.Equal(t, "1.27-bookworm", ref)
	require.False(t, repository.PlainHTTP)

	repository, ref, err = registryRepository("localhost:5000/app@sha256:" + strings.Repeat("b", 64))
	require.NoError(t, err)
	require.Equal(t, "sha256:"+strings.Repeat("b", 64), ref)
	require.True(t, repository.PlainHTTP, "a loopback registry is plain HTTP, as docker reaches it")

	_, ref, err = registryRepository("ghcr.io/org/app")
	require.NoError(t, err)
	require.Equal(t, "latest", ref)
}
