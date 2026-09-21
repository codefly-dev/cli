package composition

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	core "github.com/codefly-dev/core/composition"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

func artifactRepository(artifact *core.ReleaseArtifact) (*remote.Repository, error) {
	reference, err := registry.ParseReference(strings.TrimPrefix(artifact.URI, "oci://"))
	if err != nil || reference.Reference != artifact.Digest || artifact.MediaType == "" {
		return nil, errors.New("OCI artifact requires an explicit media type and a URI addressed by its exact selected digest")
	}
	digest, err := reference.Digest()
	if err != nil || digest.Validate() != nil {
		return nil, errors.New("OCI artifact reference must be a digest, not a mutable tag")
	}
	return remote.NewRepository(reference.Registry + "/" + reference.Repository)
}

func fetchOCIArtifact(ctx context.Context, client *http.Client, artifact *core.ReleaseArtifact) (io.ReadCloser, error) {
	repository, err := artifactRepository(artifact)
	if err != nil {
		return nil, err
	}
	repository.Client, err = registryArtifactClient(client)
	if err != nil {
		return nil, err
	}
	var fetcher registry.ReferenceFetcher = repository.Blobs()
	manifest := false
	switch artifact.MediaType {
	case ocispec.MediaTypeImageManifest, ocispec.MediaTypeImageIndex,
		"application/vnd.docker.distribution.manifest.v2+json", "application/vnd.docker.distribution.manifest.list.v2+json":
		fetcher = repository.Manifests()
		manifest = true
	}
	descriptor, reader, err := fetcher.FetchReference(ctx, artifact.Digest)
	if err != nil {
		return nil, errors.Join(ctx.Err(), errors.New("OCI acquisition failed; verify the selected digest, registry access and authorization"))
	}
	if descriptor.Digest.String() != artifact.Digest || descriptor.Size < 0 || descriptor.Size > maxSelectionArtifactBytes {
		return nil, errors.Join(errors.New("OCI artifact descriptor differs from the selected digest or exceeds the size limit"), reader.Close())
	}
	// Registry blob endpoints commonly return octet-stream regardless of signed
	// media type. For manifests, the protocol carries the actual representation.
	if manifest && descriptor.MediaType != artifact.MediaType {
		return nil, errors.Join(errors.New("OCI manifest media type differs from the selected representation"), reader.Close())
	}
	return reader, nil
}

func registryArtifactClient(client *http.Client) (*auth.Client, error) {
	store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return nil, errors.New("cannot load registry credentials")
	}
	return &auth.Client{Client: artifactHTTPClient(client), Cache: auth.NewCache(), Credential: credentials.Credential(store)}, nil
}
