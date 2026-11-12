package orchestration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/distribution/reference"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

// The reuse decision's registry reads, answered in process.
//
// Each read used to be a `docker buildx imagetools inspect` process: a CLI
// start, a builder lookup and a credential-helper run before the one HTTP round
// trip that answers, about four seconds a read, several reads per recipe. Here
// one HTTP client per process serves every read, authenticated the way docker
// is (the docker configuration and its credential helpers), each helper asked
// once per registry host.
//
// The manifest check and the labels check of one reference share one manifest
// fetch (registryManifestOf, kept for the render by cachedProbe), and the
// labels of a manifest are read once per digest ever: a digest names its
// manifest's bytes, which name its configurations' bytes, so what they say can
// never change and is kept under CODEFLY_HOME (digestLabels). What a TAG
// resolves to is never kept beyond the render: a tag moves.
//
// A caller-named builder (World.BuildxBuilder) still reads through buildx,
// because a registry reachable only from that builder is reachable nowhere else.

// registryManifest is what a registry served for one reference: the digest of
// the exact bytes, the bytes, and the media type it served them as.
type registryManifest struct {
	digest    string
	mediaType string
	raw       []byte
}

// maxRegistryManifestBytes bounds a manifest read; the distribution spec's own
// recommended limit is 4 MiB.
const maxRegistryManifestBytes = 4 << 20

// registryHTTP is the process's one registry client.
var registryHTTP = sync.OnceValues(func() (*auth.Client, error) {
	store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return nil, fmt.Errorf("load the docker registry credentials: %w", err)
	}
	return &auth.Client{
		Cache:      auth.NewCache(),
		Credential: oncePerHost(credentials.Credential(store)),
	}, nil
})

// oncePerHost asks the credential store once per registry host for a few
// minutes. A credential helper is a process (docker-credential-gcloud runs
// gcloud), and docker itself pays for it on every command; one render asks the
// same host dozens of times. The answer is short-lived because a helper's
// credential may be (an access token is), and a run can outlive it. A failure
// is not kept, so the next read asks again.
func oncePerHost(read auth.CredentialFunc) auth.CredentialFunc {
	type answer struct {
		credential auth.Credential
		at         time.Time
	}
	var mu sync.Mutex
	known := map[string]answer{}
	return func(ctx context.Context, host string) (auth.Credential, error) {
		mu.Lock()
		defer mu.Unlock()
		if kept, ok := known[host]; ok && time.Since(kept.at) < registryCredentialLifetime {
			return kept.credential, nil
		}
		credential, err := read(ctx, host)
		if err == nil {
			known[host] = answer{credential, time.Now()}
		}
		return credential, err
	}
}

// registryCredentialLifetime is how long a credential helper's answer is
// reused: well inside the hour an access token it hands out lives.
const registryCredentialLifetime = 5 * time.Minute

// registryRepository resolves an image reference the way docker does (a bare
// `golang:1.25` is docker.io/library/golang:1.25) to its repository and the
// tag or digest to fetch.
func registryRepository(image string) (*remote.Repository, string, error) {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return nil, "", fmt.Errorf("parse image reference %s: %w", image, err)
	}
	named = reference.TagNameOnly(named)
	var ref string
	switch typed := named.(type) {
	case reference.Digested:
		ref = typed.Digest().String()
	case reference.Tagged:
		ref = typed.Tag()
	}
	repository, err := remote.NewRepository(named.Name())
	if err != nil {
		return nil, "", err
	}
	client, err := registryHTTP()
	if err != nil {
		return nil, "", err
	}
	repository.Client = client
	repository.PlainHTTP = loopbackRegistry(reference.Domain(named))
	return repository, ref, nil
}

// loopbackRegistry is a registry on this machine, which docker and buildx reach
// over plain HTTP.
func loopbackRegistry(host string) bool {
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// fetchRegistryManifest reads the manifest a reference names. The digest is
// over the bytes served, exactly what `imagetools inspect --raw` printed and
// was hashed before; a registry's "no such manifest" is errImageNotFound.
func fetchRegistryManifest(ctx context.Context, image string) (registryManifest, error) {
	probe, cancel := context.WithTimeout(ctx, imageRegistryProbeTimeout)
	defer cancel()
	repository, ref, err := registryRepository(image)
	if err != nil {
		return registryManifest{}, err
	}
	descriptor, body, err := repository.Manifests().FetchReference(probe, ref)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return registryManifest{}, fmt.Errorf("%w: %s", errImageNotFound, err)
		}
		return registryManifest{}, err
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, maxRegistryManifestBytes+1))
	if err != nil {
		return registryManifest{}, fmt.Errorf("read the manifest of %s: %w", image, err)
	}
	if len(raw) > maxRegistryManifestBytes {
		return registryManifest{}, fmt.Errorf("the manifest of %s exceeds %d bytes", image, maxRegistryManifestBytes)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return registryManifest{}, fmt.Errorf("registry served no manifest for %s", image)
	}
	sum := sha256.Sum256(raw)
	return registryManifest{digest: "sha256:" + hex.EncodeToString(sum[:]), mediaType: descriptor.MediaType, raw: raw}, nil
}

// registryManifestOf is the manifest check and the labels check's one read of
// a reference, kept for the render like any probe.
func registryManifestOf(ctx context.Context, image string) (registryManifest, error) {
	return cachedProbe(ctx, "registry-manifest", image, "", func() (registryManifest, error) {
		return fetchRegistryManifest(ctx, image)
	})
}

// imageLabels reads the labels every platform of the image agrees on, as
// `imagetools inspect --format '{{json .Image}}'` did (buildLabelsFromImageConfig):
// one configuration for an image manifest, one per platform for an index,
// whose attestation manifests are not platforms.
func imageLabels(ctx context.Context, image string, manifest registryManifest) (map[string]string, error) {
	if labels, ok := digestLabels(manifest.digest); ok {
		return labels, nil
	}
	repository, _, err := registryRepository(image)
	if err != nil {
		return nil, err
	}
	probe, cancel := context.WithTimeout(ctx, imageRegistryProbeTimeout)
	defer cancel()
	var platforms []map[string]string
	if isIndexMediaType(manifest.mediaType) {
		var index ocispec.Index
		if err = json.Unmarshal(manifest.raw, &index); err != nil {
			return nil, fmt.Errorf("parse the index of %s: %w", image, err)
		}
		for i := range index.Manifests {
			child := &index.Manifests[i]
			if child.Annotations[attestationReferenceType] == attestationManifest {
				continue
			}
			labels, err := manifestConfigLabels(probe, repository, child)
			if err != nil {
				return nil, fmt.Errorf("read %s of %s: %w", child.Digest, image, err)
			}
			platforms = append(platforms, labels)
		}
	} else {
		labels, err := configLabels(probe, repository, manifest.raw)
		if err != nil {
			return nil, fmt.Errorf("read the configuration of %s: %w", image, err)
		}
		platforms = append(platforms, labels)
	}
	agreed := agreedLabels(platforms)
	storeDigestLabels(manifest.digest, agreed)
	return agreed, nil
}

// The annotation buildx marks an index's attestation manifests with.
const (
	attestationReferenceType = "vnd.docker.reference.type"
	attestationManifest      = "attestation-manifest"
)

func isIndexMediaType(mediaType string) bool {
	return mediaType == ocispec.MediaTypeImageIndex || mediaType == "application/vnd.docker.distribution.manifest.list.v2+json"
}

// manifestConfigLabels reads one platform's labels from its manifest.
func manifestConfigLabels(ctx context.Context, repository *remote.Repository, descriptor *ocispec.Descriptor) (map[string]string, error) {
	body, err := repository.Manifests().Fetch(ctx, *descriptor)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, maxRegistryManifestBytes))
	if err != nil {
		return nil, err
	}
	return configLabels(ctx, repository, raw)
}

// configLabels reads the labels of the configuration an image manifest names.
func configLabels(ctx context.Context, repository *remote.Repository, raw []byte) (map[string]string, error) {
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, err
	}
	body, err := repository.Blobs().Fetch(ctx, manifest.Config)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var config struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err = json.NewDecoder(io.LimitReader(body, maxRegistryManifestBytes)).Decode(&config); err != nil {
		return nil, err
	}
	return config.Config.Labels, nil
}

// agreedLabels keeps a label only when every platform records the same
// non-empty value.
func agreedLabels(platforms []map[string]string) map[string]string {
	agreed := map[string]string{}
	if len(platforms) == 0 {
		return agreed
	}
	for key, value := range platforms[0] {
		if value == "" {
			continue
		}
		all := true
		for _, platform := range platforms[1:] {
			if platform[key] != value {
				all = false
				break
			}
		}
		if all {
			agreed[key] = value
		}
	}
	return agreed
}

// onlyBuildLabels is the subset of labels the reuse decision reads.
func onlyBuildLabels(labels map[string]string) map[string]string {
	kept := map[string]string{}
	for _, key := range buildLabels {
		if value := labels[key]; value != "" {
			kept[key] = value
		}
	}
	return kept
}

// digestLabelsPath is where the labels of one manifest digest are kept, or ""
// when the digest is not one this can name a file by.
func digestLabelsPath(digest string) string {
	if !sha256Digest.MatchString(digest) {
		return ""
	}
	return filepath.Join(resources.CodeflyHomeDir(), "cache", "registry", "labels", strings.TrimPrefix(digest, "sha256:")+".json")
}

// digestLabels is what an earlier read recorded for this manifest digest.
func digestLabels(digest string) (map[string]string, bool) {
	path := digestLabelsPath(digest)
	if path == "" {
		return nil, false
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var record struct {
		Digest string            `json:"digest"`
		Labels map[string]string `json:"labels"`
	}
	if json.Unmarshal(payload, &record) != nil || record.Digest != digest || record.Labels == nil {
		return nil, false
	}
	return record.Labels, true
}

// storeDigestLabels records a digest's labels. A failure to write costs the
// next render one read, so it is not an error.
func storeDigestLabels(digest string, labels map[string]string) {
	path := digestLabelsPath(digest)
	if path == "" {
		return
	}
	payload, err := json.Marshal(struct {
		Digest string            `json:"digest"`
		Labels map[string]string `json:"labels"`
	}{digest, labels})
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	_ = shared.WriteFileAtomic(context.Background(), path, payload, 0o644)
}
