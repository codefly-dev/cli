package composition

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"

	core "github.com/codefly-dev/core/composition"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// PublishRuntimeArtifact pushes bytes a render built to an OCI repository as
// one content-addressed blob, roots it against registry garbage collection the
// way a build publication does (retainLayers), reads it back by digest, and
// returns the runtime release artifact naming it. The repository is the
// environment's, reached with the operator's registry credentials; the bytes
// are named by digest, so publishing the same bytes twice is the same
// artifact.
func PublishRuntimeArtifact(ctx context.Context, repositoryReference, name, mediaType string, data []byte, client *http.Client) (core.ReleaseArtifact, error) {
	if err := core.ValidateReleaseArtifactName(name); err != nil {
		return core.ReleaseArtifact{}, err
	}
	if mediaType == "" || len(data) == 0 || int64(len(data)) > maxSelectionArtifactBytes {
		return core.ReleaseArtifact{}, errors.New("a runtime artifact needs an explicit media type and non-empty bytes within the artifact size limit")
	}
	repository, err := publicationRepository(repositoryReference, client)
	if err != nil {
		return core.ReleaseArtifact{}, err
	}
	descriptor := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
	if err = repository.Blobs().Push(ctx, descriptor, bytes.NewReader(data)); err != nil {
		return core.ReleaseArtifact{}, errors.Join(ctx.Err(), errors.New("runtime artifact upload failed; verify registry authorization"))
	}
	if _, err = retainLayers(ctx, repository, "application/vnd.codefly.runtime-artifact.v1",
		map[string]string{"dev.codefly.artifact": name}, []ocispec.Descriptor{descriptor}); err != nil {
		return core.ReleaseArtifact{}, err
	}
	artifact := core.ReleaseArtifact{Name: name, Purpose: core.ArtifactRuntime, URI: "oci://" + repositoryReference + "@" + descriptor.Digest.String(),
		Digest: descriptor.Digest.String(), MediaType: mediaType}
	// Read back from the registry: an upload status is not evidence that the
	// URI the manifest names serves these bytes.
	reader, err := fetchArtifact(ctx, client, &artifact)
	if err != nil {
		return core.ReleaseArtifact{}, err
	}
	served, readErr := io.ReadAll(io.LimitReader(reader, int64(len(data))+1))
	if err = errors.Join(readErr, reader.Close()); err != nil || !bytes.Equal(served, data) {
		return core.ReleaseArtifact{}, errors.Join(err, errors.New("runtime artifact readback differs from the published bytes"))
	}
	return artifact, nil
}
